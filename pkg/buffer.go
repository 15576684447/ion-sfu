package sfu

import (
	"math"
	"sort"
	"sync"
	"time"

	"github.com/gammazero/deque"
	log "github.com/pion/ion-log"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3"
)

const (
	maxSN = 1 << 16
	// default buffer time by ms
	defaultBufferTime = 1000
)

type rtpExtInfo struct {
	ExtTSN    uint32
	Timestamp int64
}

// Buffer contains all packets
type Buffer struct {
	mu sync.RWMutex

	pktQueue   queue
	codecType  webrtc.RTPCodecType
	simulcast  bool
	clockRate  uint32
	maxBitrate uint64

	// supported feedbacks
	remb bool
	nack bool
	tcc  bool

	lastSRNTPTime      uint64
	lastSRRTPTime      uint32
	lastSRRecv         int64 // Represents wall clock of the most recent sender report arrival
	baseSN             uint16
	cycles             uint32
	lastExpected       uint32
	lastReceived       uint32
	lostRate           float32
	ssrc               uint32
	lastPacketTime     int64  // Time the last RTP packet from this source was received
	lastRtcpPacketTime int64  // Time the last RTCP packet was received.
	lastRtcpSrTime     int64  // Time the last RTCP SR was received. Required for DLSR computation.
	packetCount        uint32 // Number of packets received from this source.
	lastTransit        uint32
	maxSeqNo           uint16  // The highest sequence number received in an RTP data packet
	jitter             float64 // An estimate of the statistical variance of the RTP data packet inter-arrival time.
	totalByte          uint64

	// transport-cc
	tccExt       uint8
	tccExtInfo   []rtpExtInfo
	tccCycles    uint32
	tccLastExtSN uint32
	tccPktCtn    uint8
	tccLastSn    uint16
	lastExtInfo  uint16
}

// BufferOptions provides configuration options for the buffer
type BufferOptions struct {
	TCCExt     int
	BufferTime int
	MaxBitRate uint64
}

// NewBuffer constructs a new Buffer
func NewBuffer(track *webrtc.Track, o BufferOptions) *Buffer {
	b := &Buffer{
		ssrc:       track.SSRC(),
		clockRate:  track.Codec().ClockRate,
		codecType:  track.Codec().Type,
		maxBitrate: o.MaxBitRate,
		simulcast:  len(track.RID()) > 0,
	}
	if o.BufferTime <= 0 {
		o.BufferTime = defaultBufferTime
	}
	b.pktQueue.duration = uint32(o.BufferTime) * b.clockRate / 1000 //clockRate, audio:48000 || video:90000
	b.pktQueue.ssrc = track.SSRC()
	b.tccExt = uint8(o.TCCExt)

	for _, fb := range track.Codec().RTCPFeedback {
		switch fb.Type {
		case webrtc.TypeRTCPFBGoogREMB: // set REMB
			log.Debugf("Setting feedback %s", webrtc.TypeRTCPFBGoogREMB)
			b.remb = true
		case webrtc.TypeRTCPFBTransportCC: // set TCC
			log.Debugf("Setting feedback %s", webrtc.TypeRTCPFBTransportCC)
			b.tccExtInfo = make([]rtpExtInfo, 1<<8)
			b.tcc = true
		case webrtc.TypeRTCPFBNACK: // set NACK
			log.Debugf("Setting feedback %s", webrtc.TypeRTCPFBNACK)
			b.nack = true
		}
	}
	log.Debugf("NewBuffer BufferOptions=%v", o)
	return b
}

// Push adds a RTP Packet, out of order, new packet may be arrived later
func (b *Buffer) Push(p *rtp.Packet) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.totalByte += uint64(p.MarshalSize())
	if b.packetCount == 0 { //如果当前buffer还没任何pkt，则以该pkt初始化buffer
		b.baseSN = p.SequenceNumber
		b.maxSeqNo = p.SequenceNumber
		b.pktQueue.headSN = p.SequenceNumber - 1
	} else if snDiff(b.maxSeqNo, p.SequenceNumber) <= 0 { //如果该pkt的SequenceNumber < buffer.maxSeqNo，则开始下一个buffer loop存储
		if p.SequenceNumber < b.maxSeqNo {
			b.cycles += maxSN
		}
		b.maxSeqNo = p.SequenceNumber
	}
	b.packetCount++                                                    //pkt总数++
	b.lastPacketTime = time.Now().UnixNano()                           //当前时间记为lastPacketTime
	arrival := uint32(b.lastPacketTime / 1e6 * int64(b.clockRate/1e3)) //以clockRate为时间基，计算该pkt的到达时间(ns/1000000 * clockRate/1000 = s * clockRate/s)
	transit := arrival - p.Timestamp                                   //计算基于clockRate时间基的到达延迟
	if b.lastTransit != 0 {                                            //如果上一次的transmit不为0，计算前后两次的到达时间差
		d := int32(transit - b.lastTransit)
		if d < 0 {
			d = -d
		}
		b.jitter += (float64(d) - b.jitter) / 16 //todo: 使用相邻两次的到达时间差计算jitter
	}
	b.lastTransit = transit                      //设置为lastTransit
	if b.codecType == webrtc.RTPCodecTypeVideo { //视频pkt添加到buffer
		b.pktQueue.AddPacket(p, p.SequenceNumber == b.maxSeqNo)
	}

	if b.tcc { //如果开启了tcc
		rtpTCC := rtp.TransportCCExtension{} //获取pkt拓展头中的tcc字段，即全局sequenceNumber计数
		if err := rtpTCC.Unmarshal(p.GetExtension(b.tccExt)); err == nil {
			if rtpTCC.TransportSequence < 0x0fff && (b.tccLastSn&0xffff) > 0xf000 {
				b.tccCycles += maxSN
			}
			//tcc TransportSequence的最高一个字节用于统计tcc的cycles，所以一个cycle的tcc计数周期为4096
			//tcc计数最大周期为 2^16 = 16*4094
			//todo: 计算tcc
			b.tccExtInfo = append(b.tccExtInfo, rtpExtInfo{ //统计每个pkt的到达时间(基于clockRate单位)
				ExtTSN:    b.tccCycles | uint32(rtpTCC.TransportSequence),
				Timestamp: b.lastPacketTime / 1e3,
			})
		}
	}
}

//REMB: 根据丢包率计算带宽，构造rtcp发送至上游
/*
lostRate < 0.02: 提高码率
lostRate > 0.1: 降低码率
0.02 =< lostRate <= 0.1: 码率不变
*/
func (b *Buffer) buildREMBPacket() *rtcp.ReceiverEstimatedMaximumBitrate {
	br := b.totalByte * 8
	if b.lostRate < 0.02 {
		br = uint64(float64(br)*1.09) + 2000
	}
	if b.lostRate > .1 {
		br = uint64(float64(br) * float64(1-0.5*b.lostRate))
	}
	if br > b.maxBitrate {
		br = b.maxBitrate
	}
	if br < 100000 {
		br = 100000
	}
	b.totalByte = 0

	return &rtcp.ReceiverEstimatedMaximumBitrate{
		SenderSSRC: b.ssrc,
		Bitrate:    br,
		SSRCs:      []uint32{b.ssrc},
	}
}

func (b *Buffer) buildTransportCCPacket() *rtcp.TransportLayerCC {
	if len(b.tccExtInfo) == 0 {
		return nil
	}
	//以tcc扩展sequence排序
	sort.Slice(b.tccExtInfo, func(i, j int) bool {
		return b.tccExtInfo[i].ExtTSN < b.tccExtInfo[j].ExtTSN
	})
	tccPkts := make([]rtpExtInfo, 0, int(float64(len(b.tccExtInfo))*1.2))
	//遍历tccExtInfo，处理所有的tcc pkt
	//中间可能存在丢包情况，所以tccExtInfo.ExtTSN不一定是连续的
	for _, tccExtInfo := range b.tccExtInfo {
		if tccExtInfo.ExtTSN < b.tccLastExtSN {
			continue
		}
		//todo: 如果两个ExtTSN之间存在丢包，则为一个一个片段，此时需要补齐丢包位置的rtpExtInfo
		//todo: 如果LastExtSN到当前ExtTSN存在丢包，则会将构建丢包的rtpExtInfo加入到tccPkts
		if b.tccLastExtSN != 0 {
			for j := b.tccLastExtSN + 1; j < tccExtInfo.ExtTSN; j++ {
				tccPkts = append(tccPkts, rtpExtInfo{ExtTSN: j})
			}
		}
		//将当前ExtTSN设置为tccLastExtSN
		b.tccLastExtSN = tccExtInfo.ExtTSN
		//将当前tccExtInfo加入到tccPkts
		tccPkts = append(tccPkts, tccExtInfo)
	}
	//todo: 此时tccPkts中的tcc包肯定是连续的，丢包位置会被补齐
	//清空tcc信息
	b.tccExtInfo = b.tccExtInfo[:0]
	//组装transport tcc
	rtcpTCC := &rtcp.TransportLayerCC{
		Header: rtcp.Header{
			Padding: true,
			Count:   rtcp.FormatTCC,
			Type:    rtcp.TypeTransportSpecificFeedback,
		},
		MediaSSRC:          b.ssrc,
		BaseSequenceNumber: uint16(tccPkts[0].ExtTSN),
		PacketStatusCount:  uint16(len(tccPkts)),
		FbPktCount:         b.tccPktCtn,
	}
	b.tccPktCtn++

	firstRecv := false
	allSame := true
	timestamp := int64(0)
	deltaLen := 0
	lastStatus := rtcp.TypeTCCPacketReceivedWithoutDelta
	maxStatus := rtcp.TypeTCCPacketNotReceived

	var statusList deque.Deque
	//遍历所有tccPkts
	for _, stat := range tccPkts {
		status := rtcp.TypeTCCPacketNotReceived
		//统计每个tcc pkt的接收延迟 delta，根据 delta 大小，确定使用small还是large模式
		if stat.Timestamp != 0 {
			var delta int64
			if !firstRecv {
				firstRecv = true
				timestamp = stat.Timestamp
				//第一个tccPkt的时间作为ReferenceTime
				rtcpTCC.ReferenceTime = uint32(stat.Timestamp / 64000)
			}
			//计算相邻tccPkt的delta
			delta = (stat.Timestamp - timestamp) / 250
			if delta < 0 || delta > 255 {
				status = rtcp.TypeTCCPacketReceivedLargeDelta
				rDelta := int16(delta)
				if int64(rDelta) != delta {
					if rDelta > 0 {
						rDelta = math.MaxInt16
					} else {
						rDelta = math.MinInt16
					}
				}
				//统计所有delta
				rtcpTCC.RecvDeltas = append(rtcpTCC.RecvDeltas, &rtcp.RecvDelta{
					Type:  status,
					Delta: int64(rDelta) * 250,
				})
				deltaLen += 2
			} else {
				status = rtcp.TypeTCCPacketReceivedSmallDelta
				//统计所有delta
				rtcpTCC.RecvDeltas = append(rtcpTCC.RecvDeltas, &rtcp.RecvDelta{
					Type:  status,
					Delta: delta * 250,
				})
				deltaLen++
			}
			timestamp = stat.Timestamp
		}
		//统计是否全部为同一种模式 TypeTCCPacketReceivedLargeDelta || TypeTCCPacketReceivedSmallDelta
		//todo: 如果模式不一致，则处理(模式不一致指的是需要使用不同长度字长表示delta)
		if allSame && lastStatus != rtcp.TypeTCCPacketReceivedWithoutDelta && status != lastStatus {
			//如果第一次出现status不一致时，已经存在了至少8个包，则将这些包封装成chunk，继续处理
			if statusList.Len() > 7 {
				rtcpTCC.PacketChunks = append(rtcpTCC.PacketChunks, &rtcp.RunLengthChunk{
					PacketStatusSymbol: lastStatus,
					RunLength:          uint16(statusList.Len()),
				})
				statusList.Clear()
				lastStatus = rtcp.TypeTCCPacketReceivedWithoutDelta
				maxStatus = rtcp.TypeTCCPacketNotReceived
				allSame = true
			} else {
				//如果当前不足8个包，则只能置allSame = false，todo: 等待下面流程处理
				allSame = false
			}
		}
		//暂存status
		statusList.PushBack(status)
		//统计占字节最长的status
		if status > maxStatus {
			maxStatus = status
		}
		lastStatus = status
		//todo: 如果delta模式出现不一致，则进行处理
		if !allSame {
			//如果占最长字节的status为large delta && 总数=7
			if maxStatus == rtcp.TypeTCCPacketReceivedLargeDelta && statusList.Len() > 6 {
				symbolList := make([]uint16, 7)
				for i := 0; i < 7; i++ {
					symbolList[i] = statusList.PopFront().(uint16)
				}
				//对这7个rtcp进行chunk封包
				rtcpTCC.PacketChunks = append(rtcpTCC.PacketChunks, &rtcp.StatusVectorChunk{
					SymbolSize: rtcp.TypeTCCSymbolSizeTwoBit,
					SymbolList: symbolList,
				})
				lastStatus = rtcp.TypeTCCPacketReceivedWithoutDelta
				maxStatus = rtcp.TypeTCCPacketNotReceived
				allSame = true
				//剩余部分统计maxStatus 和 allSame
				for i := 0; i < statusList.Len(); i++ {
					status = statusList.At(i).(uint16)
					if status > maxStatus {
						maxStatus = status
					}
					if allSame && lastStatus != rtcp.TypeTCCPacketReceivedWithoutDelta && status != lastStatus {
						allSame = false
					}
					lastStatus = status
				}
				//如果占最长字节的status为small delta && 总数=14
			} else if statusList.Len() > 13 {
				symbolList := make([]uint16, 14)
				for i := 0; i < 14; i++ {
					symbolList[i] = statusList.PopFront().(uint16)
				}
				//对这14个rtcp进行chunk封包
				rtcpTCC.PacketChunks = append(rtcpTCC.PacketChunks, &rtcp.StatusVectorChunk{
					SymbolSize: rtcp.TypeTCCSymbolSizeOneBit,
					SymbolList: symbolList,
				})
				lastStatus = rtcp.TypeTCCPacketReceivedWithoutDelta
				maxStatus = rtcp.TypeTCCPacketNotReceived
				allSame = true
			}
		}
	}
	//处理剩余的statusList
	//如果字长都一样，则全部封装成chunk
	//如果不一样，则以最大字节原则封装成chunk
	if statusList.Len() > 0 {
		if allSame {
			rtcpTCC.PacketChunks = append(rtcpTCC.PacketChunks, &rtcp.RunLengthChunk{
				PacketStatusSymbol: lastStatus,
				RunLength:          uint16(statusList.Len()),
			})
		} else if maxStatus == rtcp.TypeTCCPacketReceivedLargeDelta {
			symbolList := make([]uint16, statusList.Len())
			for i := 0; i < statusList.Len(); i++ {
				symbolList[i] = statusList.PopFront().(uint16)
			}
			rtcpTCC.PacketChunks = append(rtcpTCC.PacketChunks, &rtcp.StatusVectorChunk{
				SymbolSize: rtcp.TypeTCCSymbolSizeTwoBit,
				SymbolList: symbolList,
			})
		} else {
			symbolList := make([]uint16, statusList.Len())
			for i := 0; i < statusList.Len(); i++ {
				symbolList[i] = statusList.PopFront().(uint16)
			}
			rtcpTCC.PacketChunks = append(rtcpTCC.PacketChunks, &rtcp.StatusVectorChunk{
				SymbolSize: rtcp.TypeTCCSymbolSizeOneBit,
				SymbolList: symbolList,
			})
		}
	}

	pLen := uint16(20 + len(rtcpTCC.PacketChunks)*2 + deltaLen)
	rtcpTCC.Header.Padding = pLen%4 != 0
	for pLen%4 != 0 {
		pLen++
	}
	rtcpTCC.Header.Length = (pLen / 4) - 1
	return rtcpTCC
}

//构建RR
/*
本次应收
本次实际收
本次实际收 / 本次应收 = 丢包率

本次应收 - 上次应收 = 应收interval
本次实际收 - 上次实际收 = 实际收interval
实际收interval / 应收interval = 相对丢包率
*/
func (b *Buffer) buildReceptionReport() rtcp.ReceptionReport {
	extMaxSeq := b.cycles | uint32(b.maxSeqNo)
	expected := extMaxSeq - uint32(b.baseSN) + 1
	//计算丢包个数 = 理论应收到个数 - 实际收到个数
	lost := expected - b.packetCount
	if b.packetCount == 0 {
		lost = 0
	}
	//本次预期收到与上次预期收到的差值interval
	expectedInterval := expected - b.lastExpected
	b.lastExpected = expected
	//本次实际收到与上次实际收到的的差值interval
	receivedInterval := b.packetCount - b.lastReceived
	b.lastReceived = b.packetCount
	//计算相比上次，本次丢包的增量 lostInterval
	lostInterval := expectedInterval - receivedInterval
	//计算相比上次，本次丢包的丢包率增量 lostRate
	b.lostRate = float32(lostInterval) / float32(expectedInterval)
	var fracLost uint8
	if expectedInterval != 0 && lostInterval > 0 {
		fracLost = uint8((lostInterval << 8) / expectedInterval)
	}
	var dlsr uint32
	//计算距离上次收到SR的Delay
	if b.lastSRRecv != 0 {
		delayMS := uint32((time.Now().UnixNano() - b.lastSRRecv) / 1e6)
		dlsr = (delayMS / 1e3) << 16
		dlsr |= (delayMS % 1e3) * 65536 / 1000
	}
	//发送RR
	rr := rtcp.ReceptionReport{
		SSRC:               b.ssrc,
		FractionLost:       fracLost,
		TotalLost:          lost,
		LastSequenceNumber: extMaxSeq,
		Jitter:             uint32(b.jitter),
		LastSenderReport:   uint32(b.lastSRNTPTime >> 16),
		Delay:              dlsr,
	}
	return rr
}

//设置收到SR的时间
func (b *Buffer) setSenderReportData(rtpTime uint32, ntpTime uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lastSRRTPTime = rtpTime
	b.lastSRNTPTime = ntpTime
	b.lastSRRecv = time.Now().UnixNano()
}

func (b *Buffer) getRTCP() (rtcp.ReceptionReport, []rtcp.Packet) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	var pkts []rtcp.Packet
	var report rtcp.ReceptionReport
	//构造RR
	report = b.buildReceptionReport()
	//构造ERMB
	if b.remb {
		pkts = append(pkts, b.buildREMBPacket())
	}
	//构造tcc
	if b.tcc {
		if tccPkt := b.buildTransportCCPacket(); tccPkt != nil {
			pkts = append(pkts, tccPkt)
		}
	}

	return report, pkts
}

// WritePacket write buffer packet to requested track. and modify headers
func (b *Buffer) WritePacket(sn uint16, track *webrtc.Track, snOffset uint16, tsOffset, ssrc uint32) error {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if bufferPkt := b.pktQueue.GetPacket(sn); bufferPkt != nil {
		bSsrc := bufferPkt.SSRC
		bufferPkt.SequenceNumber -= snOffset
		bufferPkt.Timestamp -= tsOffset
		bufferPkt.SSRC = ssrc
		err := track.WriteRTP(bufferPkt)
		bufferPkt.Timestamp += tsOffset
		bufferPkt.SequenceNumber += snOffset
		bufferPkt.SSRC = bSsrc
		return err
	}
	return errPacketNotFound
}

func (b *Buffer) onLostHandler(fn func(nack *rtcp.TransportLayerNack)) {
	if b.nack {
		b.pktQueue.onLost = fn
	}
}
