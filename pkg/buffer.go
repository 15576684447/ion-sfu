package sfu

import (
	"fmt"
	"sync"

	"github.com/pion/ion-sfu/pkg/log"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
)

const (
	maxSN      = 65536
	maxPktSize = 1000

	// kProcessIntervalMs=20 ms
	// https://chromium.googlesource.com/external/webrtc/+/ad34dbe934/webrtc/modules/video_coding/nack_module.cc#28

	// vp8 vp9 h264 clock rate 90000Hz
	videoClock = 90000

	// 1+16(FSN+BLP) https://tools.ietf.org/html/rfc2032#page-9
	maxNackLostSize = 17

	// default buffer time by ms
	defaultBufferTime = 1000
)

func tsDelta(x, y uint32) uint32 {
	if x > y {
		return x - y
	}
	return y - x
}

/*
todo: buffer几个重点功能
	1、pktBuffer循环存放pkt，范围为0~65535,SequenceNumber为int16类型,也为65535
	2、清理过时buffer: 如果上次清理位置的buffer时间戳与当前时间戳差值大于1s，则需要清理过时位置的buffer，并更改lastClearTS/lastClearSN
	3、每16个pkt，发送一组Nack，刚好使用一个uint16变量存储，丢包位置对应bit置1
	4、丢包统计并调整带宽，反馈给发送端(REMB 定时统计)
	5、计算transport-cc-feedback，并反馈给发送端，10ms统计一次
*/
// Buffer contains all packets
type Buffer struct {
	pktBuffer   [maxSN]*rtp.Packet
	lastNackSN  uint16 //上次Nack发送位置，16个连续pkt为一组，使用一个uint16的16bit表示，如果对应位置丢包，置1
	lastClearTS uint32 //上次清理buffer的时间，如果当前时间与lastClearTS相比大于1s，则从上次清理的位置开始遍历，清理过期pkt
	lastClearSN uint16 //上次清理buffer的位置

	// Last seqnum that has been added to buffer
	lastPushSN uint16 //上次push的位置，即刚push存放的位置

	ssrc        uint32
	payloadType uint8

	// calc lost rate
	receivedPkt int //收到总pkt数
	lostPkt     int //丢失总pkt数

	// response nack channel
	//接收端发送rtcp给发送端的缓冲区
	rtcpCh chan rtcp.Packet

	// calc bandwidth
	totalByte uint64 //接收总字节数

	// buffer time
	maxBufferTS uint32 //数据缓冲时间，超过这个时间的数据被认为是过期数据，则给予清理，默认为1s

	stop bool
	mu   sync.RWMutex

	// lastTCCSN      uint16
	// bufferStartTS time.Time
}

// BufferOptions provides configuration options for the buffer
type BufferOptions struct {
	BufferTime int
}

// NewBuffer constructs a new Buffer
func NewBuffer(ssrc uint32, pt uint8, o BufferOptions) *Buffer {
	b := &Buffer{
		ssrc:        ssrc,
		payloadType: pt,
		rtcpCh:      make(chan rtcp.Packet, maxPktSize),
	}

	if o.BufferTime <= 0 {
		o.BufferTime = defaultBufferTime
	}
	b.maxBufferTS = uint32(o.BufferTime) * videoClock / 1000
	// b.bufferStartTS = time.Now()
	log.Debugf("NewBuffer BufferOptions=%v", o)
	return b
}

// Push adds a RTP Packet, out of order, new packet may be arrived later
//TODO:SequenceNumber为int16类型，0~65535，和pktBuffer等长
func (b *Buffer) Push(p *rtp.Packet) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.receivedPkt++
	b.totalByte += uint64(p.MarshalSize())

	// init ssrc payloadType
	if b.ssrc == 0 || b.payloadType == 0 {
		b.ssrc = p.SSRC
		b.payloadType = p.PayloadType
	}

	// init lastClearTS
	if b.lastClearTS == 0 {
		b.lastClearTS = p.Timestamp
	}

	// init lastClearSN
	if b.lastClearSN == 0 {
		b.lastClearSN = p.SequenceNumber
	}

	// init lastNackSN
	if b.lastNackSN == 0 {
		b.lastNackSN = p.SequenceNumber
	}
	//根据SequenceNumber存放pkt
	b.pktBuffer[p.SequenceNumber] = p
	b.lastPushSN = p.SequenceNumber
	//当前push的序列号 - 上次没有NCK的序列号
	//控制单次NCK的序列长度
	if b.lastPushSN-b.lastNackSN >= maxNackLostSize {
		// limit nack range
		b.lastNackSN = b.lastPushSN - maxNackLostSize
		// calc [lastNackSN, lastpush-8] if has keyframe
		nackPair, lostPkt := b.GetNackPair(b.pktBuffer, b.lastNackSN, b.lastPushSN)
		// clear old packet by timestamp
		//TODO:清理缓存过时的buffer，默认为1s；所以整个buffer是一个边存储新数据，边清理过时数据的过程
		b.clearOldPkt(p.Timestamp, p.SequenceNumber)
		b.lastNackSN = b.lastPushSN
		log.Tracef("b.lastNackSN=%v, b.lastPushSN=%v, lostPkt=%v, nackPair=%v", b.lastNackSN, b.lastPushSN, lostPkt, nackPair)
		//如果有丢包，则发送NACK要求对应位置重传
		if lostPkt > 0 {
			b.lostPkt += lostPkt
			nack := &rtcp.TransportLayerNack{
				// origin ssrc
				// SenderSSRC: b.ssrc,
				MediaSSRC: b.ssrc,
				Nacks: []rtcp.NackPair{
					nackPair,
				},
			}
			b.rtcpCh <- nack
		}
	}
}

// clearOldPkt clear old packet
//清理过时的buffer，如果当前pkt和上次清理的pkt时间戳差值大于maxBufferTS，则定义为过时的buffer，则清理其内容
func (b *Buffer) clearOldPkt(pushPktTS uint32, pushPktSN uint16) {
	clearTS := b.lastClearTS
	clearSN := b.lastClearSN
	log.Tracef("clearOldPkt pushPktTS=%d pushPktSN=%d     clearTS=%d  clearSN=%d ", pushPktTS, pushPktSN, clearTS, clearSN)
	if tsDelta(pushPktTS, clearTS) >= b.maxBufferTS {
		// pushPktSN will loop from 0 to 65535
		if pushPktSN == 0 {
			// make sure clear the old packet from 655xx to 65535
			pushPktSN = maxSN - 1
		}
		var skipCount int
		//遍历从上次清理的位置到现在插入的位置，计算时间差，如果超过maxBufferTS，则给予清理
		for i := clearSN + 1; i <= pushPktSN; i++ {
			if b.pktBuffer[i] == nil {
				skipCount++
				continue
			}
			if tsDelta(pushPktTS, b.pktBuffer[i].Timestamp) >= b.maxBufferTS {
				b.lastClearTS = b.pktBuffer[i].Timestamp
				b.lastClearSN = i
				b.pktBuffer[i] = nil
			} else {
				break
			}
		}
		if skipCount > 0 {
			log.Tracef("b.pktBuffer nil count : %d", skipCount)
		}
		if pushPktSN == maxSN-1 {
			b.lastClearSN = 0
			b.lastNackSN = 0
		}
	}
}

// Stop buffer
func (b *Buffer) Stop() {
	b.stop = true
	close(b.rtcpCh)
	b.clear()
}

func (b *Buffer) clear() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for i := range b.pktBuffer {
		b.pktBuffer[i] = nil
	}
}

// GetPayloadType get payloadtype
func (b *Buffer) GetPayloadType() uint8 {
	return b.payloadType
}

func (b *Buffer) stats() string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return fmt.Sprintf("buffer: [%d, %d] | lastNackSN: %d", b.lastClearSN, b.lastPushSN, b.lastNackSN)
}

// GetNackPair calc nackpair
func (b *Buffer) GetNackPair(buffer [65536]*rtp.Packet, begin, end uint16) (rtcp.NackPair, int) {
	var lostPkt int

	// size is <= 17
	if end-begin > maxNackLostSize {
		return rtcp.NackPair{}, lostPkt
	}

	// Bitmask of following lost packets (BLP)
	blp := uint16(0)
	lost := uint16(0)

	// find first lost pkt
	//TODO:这里没有到end，即总数为maxNackLostSize-1 = 16，刚好对应uint16
	for i := begin; i < end; i++ {
		if buffer[i] == nil {
			lost = i
			lostPkt++
			break
		}
	}

	// no packet lost
	if lost == 0 {
		return rtcp.NackPair{}, lostPkt
	}

	// calc blp
	//TODO:用一个16bit的uint16，表示连续16个位置，哪些位置有pkt丢失!!!
	for i := lost; i < end; i++ {
		// calc from next lost packet
		if i > lost && buffer[i] == nil {
			blp |= (1 << (i - lost - 1))
			lostPkt++
		}
	}
	// log.Tracef("NackPair begin=%v end=%v buffer=%v\n", begin, end, buffer[begin:end])
	return rtcp.NackPair{PacketID: lost, LostPackets: rtcp.PacketBitmap(blp)}, lostPkt
}

// GetSSRC get ssrc
func (b *Buffer) GetSSRC() uint32 {
	return b.ssrc
}

// GetRTCPChan return rtcp channel
func (b *Buffer) GetRTCPChan() chan rtcp.Packet {
	return b.rtcpCh
}

// GetLostRateBandwidth calc lostRate and bandwidth by cycle
func (b *Buffer) GetLostRateBandwidth(cycle uint64) (float64, uint64) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	//计算丢包率
	lostRate := float64(b.lostPkt) / float64(b.receivedPkt+b.lostPkt)
	//计算平均码率
	byteRate := b.totalByte / cycle
	log.Tracef("Buffer.CalcLostRateByteRate b.receivedPkt=%d b.lostPkt=%d   lostRate=%v byteRate=%v", b.receivedPkt, b.lostPkt, lostRate, byteRate)
	b.receivedPkt, b.lostPkt, b.totalByte = 0, 0, 0
	return lostRate, byteRate * 8
}

// GetPacket get packet by sequence number
func (b *Buffer) GetPacket(sn uint16) *rtp.Packet {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.pktBuffer[sn]
}
