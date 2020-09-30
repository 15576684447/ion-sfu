package sfu

import (
	"context"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/ion-sfu/pkg/log"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3"
)

const (
	// bandwidth range(kbps)
	// minBandwidth = 200
	maxSize = 1024

	// tcc stuff
	tccExtMapID = 3
	// 64ms = 64000us = 250 << 8
	// https://webrtc.googlesource.com/src/webrtc/+/f54860e9ef0b68e182a01edc994626d21961bc4b/modules/rtp_rtcp/source/rtcp_packet/transport_feedback.cc#41
	baseScaleFactor = 64000
	// https://webrtc.googlesource.com/src/webrtc/+/f54860e9ef0b68e182a01edc994626d21961bc4b/modules/rtp_rtcp/source/rtcp_packet/transport_feedback.cc#43
	timeWrapPeriodUs = (int64(1) << 24) * baseScaleFactor
)

type rtpExtInfo struct {
	// transport sequence num
	TSN       uint16
	Timestamp int64
}

// Receiver defines a interface for a track receivers
type Receiver interface {
	Track() *webrtc.Track
	AddSender(sender Sender)
	DeleteSender(pid string)
	GetPacket(sn uint16) *rtp.Packet
	ReadRTP() chan *rtp.Packet
	ReadRTCP() chan rtcp.Packet
	WriteRTCP(rtcp.Packet) error
	OnCloseHandler(fn func())
	SpatialLayer() uint8
	Close()
	stats() string
}

// WebRTCReceiver receives a video track
type WebRTCReceiver struct {
	sync.RWMutex
	ctx            context.Context
	cancel         context.CancelFunc
	buffer         *Buffer
	track          *webrtc.Track
	bandwidth      uint64
	lostRate       float64
	rtpCh          chan *rtp.Packet
	rtcpCh         chan rtcp.Packet
	rtpExtInfoChan chan rtpExtInfo
	onCloseHandler func()
	senders        map[string]Sender

	spatialLayer uint8

	maxBandwidth uint64
	maxNackTime  int64
	lastNack     int64
	feedback     string
	wg           sync.WaitGroup
}

// WebRTCVideoReceiverConfig .
type WebRTCVideoReceiverConfig struct {
	REMBCycle       int `mapstructure:"rembcycle"`
	TCCCycle        int `mapstructure:"tcccycle"`
	MaxBufferTime   int `mapstructure:"maxbuffertime"`
	ReceiveRTPCycle int `mapstructure:"rtpcycle"`
}

// NewWebRTCReceiver creates a new webrtc track receivers
func NewWebRTCReceiver(ctx context.Context, track *webrtc.Track, config RouterConfig) Receiver {
	ctx, cancel := context.WithCancel(ctx)

	w := &WebRTCReceiver{
		ctx:            ctx,
		cancel:         cancel,
		track:          track,
		senders:        make(map[string]Sender),
		rtpCh:          make(chan *rtp.Packet, maxSize),
		rtpExtInfoChan: make(chan rtpExtInfo, maxSize),
		lastNack:       time.Now().Unix(),
		maxNackTime:    config.MaxNackTime,
	}
	//todo: layer参数？？？
	switch w.track.RID() {
	case quarterResolution:
		w.spatialLayer = 1
	case halfResolution:
		w.spatialLayer = 2
	case fullResolution:
		w.spatialLayer = 3
	default:
		w.spatialLayer = 0
	}

	waitStart := make(chan struct{})
	switch track.Kind() {
	case webrtc.RTPCodecTypeVideo:
		go startVideoReceiver(w, waitStart, config)
	case webrtc.RTPCodecTypeAudio:
		go startAudioReceiver(w, waitStart)
	}
	<-waitStart
	return w
}

// OnCloseHandler method to be called on remote tracked removed
func (w *WebRTCReceiver) OnCloseHandler(fn func()) {
	w.onCloseHandler = fn
}

func (w *WebRTCReceiver) AddSender(sender Sender) {
	w.Lock()
	defer w.Unlock()
	w.senders[sender.ID()] = sender
}

func (w *WebRTCReceiver) DeleteSender(pid string) {
	w.Lock()
	defer w.Unlock()
	delete(w.senders, pid)
}

func (w *WebRTCReceiver) SpatialLayer() uint8 {
	return w.spatialLayer
}

// ReadRTP read rtp packets
func (w *WebRTCReceiver) ReadRTP() chan *rtp.Packet {
	return w.rtpCh
}

// ReadRTCP read rtcp packets
func (w *WebRTCReceiver) ReadRTCP() chan rtcp.Packet {
	return w.rtcpCh
}

// WriteRTCP write rtcp packet
func (w *WebRTCReceiver) WriteRTCP(pkt rtcp.Packet) error {
	if w.ctx.Err() != nil || w.rtcpCh == nil {
		return io.ErrClosedPipe
	}
	if _, ok := pkt.(*rtcp.TransportLayerNack); ok && w.maxNackTime > 0 {
		ln := atomic.LoadInt64(&w.lastNack)
		if (time.Now().Unix() - ln) < w.maxNackTime {
			return nil
		}
		atomic.StoreInt64(&w.lastNack, time.Now().Unix())
	}
	w.rtcpCh <- pkt
	return nil
}

// Track returns receivers track
func (w *WebRTCReceiver) Track() *webrtc.Track {
	return w.track
}

// GetPacket get a buffered packet if we have one
func (w *WebRTCReceiver) GetPacket(sn uint16) *rtp.Packet {
	if w.buffer == nil || w.ctx.Err() != nil {
		return nil
	}
	return w.buffer.GetPacket(sn)
}

// Close gracefully close the track
func (w *WebRTCReceiver) Close() {
	if w.ctx.Err() != nil {
		return
	}
	w.cancel()
}

// receiveRTP receive all incoming tracks' rtp and sent to one channel
func (w *WebRTCReceiver) receiveRTP() {
	defer w.wg.Done()
	for {
		pkt, err := w.track.ReadRTP()
		// EOF signal received, this means that the remote track has been removed
		// or the peer has been disconnected. The router must be gracefully shutdown,
		// waiting for all the receivers routines to stop.
		if err == io.EOF {
			w.Close()
			return
		}

		if err != nil {
			log.Errorf("rtp err => %v", err)
			continue
		}
		//pkt添加到buffer
		w.buffer.Push(pkt)
		//todo：如果开启transport-cc，则则存储到达时间 => 需要进一步解析
		if w.feedback == webrtc.TypeRTCPFBTransportCC {
			// store arrival time
			timestampUs := time.Now().UnixNano() / 1000
			rtpTCC := rtp.TransportCCExtension{}
			err = rtpTCC.Unmarshal(pkt.GetExtension(tccExtMapID))
			if err == nil {
				// if time.Now().Sub(b.bufferStartTS) > time.Second {

				// only calc the packet which rtpTCC.TransportSequence > b.lastTCCSN
				// https://webrtc.googlesource.com/src/webrtc/+/f54860e9ef0b68e182a01edc994626d21961bc4b/modules/rtp_rtcp/source/rtcp_packet/transport_feedback.cc#353
				// if rtpTCC.TransportSequence > b.lastTCCSN {
				w.rtpExtInfoChan <- rtpExtInfo{
					TSN:       rtpTCC.TransportSequence,
					Timestamp: timestampUs,
				}
				// b.lastTCCSN = rtpTCC.TransportSequence
				// }
			}
		}

		select {
		case <-w.ctx.Done():
			return
		default:
			w.rtpCh <- pkt
		}
	}
}

func (w *WebRTCReceiver) fwdRTP() {
	for pkt := range w.rtpCh {
		// Push to sub send queues
		w.RLock()
		for _, sub := range w.senders {
			sub.WriteRTP(pkt)
		}
		w.RUnlock()
	}
}

func (w *WebRTCReceiver) bufferRtcpLoop() {
	defer w.wg.Done()
	for {
		select {
		case pkt := <-w.buffer.GetRTCPChan():
			w.rtcpCh <- pkt
		case <-w.ctx.Done():
			return
		}
	}
}

//接收端带宽估计并反馈到发送端
func (w *WebRTCReceiver) rembLoop(cycle int) {
	defer w.wg.Done()
	if cycle <= 0 {
		cycle = 1
	}
	t := time.NewTicker(time.Duration(cycle) * time.Second)

	for {
		select {
		case <-t.C:
			// only calc video recently
			w.lostRate, w.bandwidth = w.buffer.GetLostRateBandwidth(uint64(cycle))
			var bw uint64
			/*
				基于丢包的带宽估计(kb)
				1、首先当启动时，此时没有带宽，则以最大带宽进行传输；
				2、当丢包率大于10%时则认为网络有拥塞，此时根据丢包率降低带宽，丢包率越高带宽降的越多；
				3、当丢包率在10%内时则网络状态良好，此时将带宽提升到之前的两倍；
			*/
			switch {
			case w.lostRate == 0 && w.bandwidth == 0:
				bw = w.maxBandwidth
			case w.lostRate >= 0 && w.lostRate < 0.1:
				bw = w.bandwidth * 2
			default:
				bw = uint64(float64(w.bandwidth) * (1 - w.lostRate))
			}

			if bw > w.maxBandwidth && w.maxBandwidth > 0 {
				bw = w.maxBandwidth
			}
			//这是带宽估计的早期实现，评估的带宽结果通过RTCP REMB消息反馈到发送端
			//在新近的WebRTC的实现中，所有的带宽估计都放在了发送端
			remb := &rtcp.ReceiverEstimatedMaximumBitrate{
				SenderSSRC: w.buffer.GetSSRC(),
				Bitrate:    bw,
				SSRCs:      []uint32{w.buffer.GetSSRC()},
			}
			w.rtcpCh <- remb
		case <-w.ctx.Done():
			t.Stop()
			return
		}
	}
}

//计算transport-cc-feedback
func (w *WebRTCReceiver) tccLoop(cycle int) {
	defer w.wg.Done()
	feedbackPacketCount := uint8(0)
	t := time.NewTicker(time.Duration(cycle) * time.Millisecond)
	for {
		select {
		case <-t.C:
			cp := len(w.rtpExtInfoChan)
			if cp == 0 {
				continue
			}

			// get all rtp extension infos from channel
			//将一段时间内的所有媒体包信息按照sequence-num排序
			rtpExtInfo := make(map[uint16]int64)
			for i := 0; i < cp; i++ {
				info := <-w.rtpExtInfoChan
				rtpExtInfo[info.TSN] = info.Timestamp
			}

			// find the min and max transport sn
			var minTSN, maxTSN uint16
			for tsn := range rtpExtInfo {

				// init
				if minTSN == 0 {
					minTSN = tsn
				}

				if minTSN > tsn {
					minTSN = tsn
				}

				if maxTSN < tsn {
					maxTSN = tsn
				}
			}

			// force small deta rtcp.RunLengthChunk
			//transport-cc载体数据(表示媒体包到达状态的结构)编码格式选择RunLengthChunk方式
			//force small deta rtcp.RunLengthChunk
			chunk := &rtcp.RunLengthChunk{
				Type:               rtcp.TypeTCCRunLengthChunk,
				PacketStatusSymbol: rtcp.TypeTCCPacketReceivedSmallDelta,
				RunLength:          maxTSN - minTSN + 1,
			}

			// gather deltas
			var recvDeltas []*rtcp.RecvDelta
			//基准时间，计算该包中每个媒体包的到达时间都要基于这个基准时间计算
			var refTime uint32
			var lastTS int64
			var baseTimeTicks int64
			for i := minTSN; i <= maxTSN; i++ {
				ts, ok := rtpExtInfo[i]

				// lost packet
				//如果对应位置没有媒体包信息，说明对应序列位置包丢失
				if !ok {
					recvDelta := &rtcp.RecvDelta{
						Type: rtcp.TypeTCCPacketReceivedSmallDelta,
					}
					recvDeltas = append(recvDeltas, recvDelta)
					continue
				}

				// init lastTS
				if lastTS == 0 {
					lastTS = ts
				}

				// received packet
				if baseTimeTicks == 0 {
					baseTimeTicks = (ts % timeWrapPeriodUs) / baseScaleFactor
				}
				//将所有后续包的时间戳减去第一个包的时间戳，得到delta
				var delta int64
				if lastTS == ts {
					delta = ts%timeWrapPeriodUs - baseTimeTicks*baseScaleFactor
				} else {
					delta = (ts - lastTS) % timeWrapPeriodUs
				}

				if refTime == 0 {
					refTime = uint32(baseTimeTicks) & 0x007FFFFF
				}

				recvDelta := &rtcp.RecvDelta{
					Type:  rtcp.TypeTCCPacketReceivedSmallDelta,
					Delta: delta,
				}
				recvDeltas = append(recvDeltas, recvDelta)
			}
			rtcpTCC := &rtcp.TransportLayerCC{
				Header: rtcp.Header{
					Padding: false,
					Count:   rtcp.FormatTCC,
					Type:    rtcp.TypeTransportSpecificFeedback,
					// Length:  5, //need calc
				},
				// SenderSSRC:         w.ssrc,
				MediaSSRC:          w.track.SSRC(),
				BaseSequenceNumber: minTSN,                          //当前媒体包序列开始位置
				PacketStatusCount:  maxTSN - minTSN + 1,             //当前媒体序列包总数
				ReferenceTime:      refTime,                         //基准时间，计算该包中每个媒体包的到达时间都要基于这个基准时间计算
				FbPktCount:         feedbackPacketCount,             //第几个transport-cc包
				RecvDeltas:         recvDeltas,                      //该媒体包计算的所有时间差序列
				PacketChunks:       []rtcp.PacketStatusChunk{chunk}, //媒体包信息编码类型(共两种，这里选择RunLengthChunk方式)
			}
			rtcpTCC.Header.Length = rtcpTCC.Len()/4 - 1
			w.rtcpCh <- rtcpTCC
			feedbackPacketCount++
		case <-w.ctx.Done():
			t.Stop()
			return
		}
	}
}

// Stats get stats for video receivers
func (w *WebRTCReceiver) stats() string {
	switch w.track.Kind() {
	case webrtc.RTPCodecTypeVideo:
		return fmt.Sprintf("payload: %d | lostRate: %.2f | bandwidth: %dkbps | %s", w.buffer.GetPayloadType(), w.lostRate, w.bandwidth/1000, w.buffer.stats())
	case webrtc.RTPCodecTypeAudio:
		return fmt.Sprintf("payload: %d", w.track.PayloadType())
	default:
		return ""
	}
}

func startVideoReceiver(w *WebRTCReceiver, wStart chan struct{}, config RouterConfig) {
	defer func() {
		w.buffer.Stop()
		close(w.rtpCh)
		close(w.rtcpCh)
		if w.onCloseHandler != nil {
			w.onCloseHandler()
		}
	}()

	w.rtcpCh = make(chan rtcp.Packet, maxSize)
	//为receiver设置buffer
	w.buffer = NewBuffer(w.track.SSRC(), w.track.PayloadType(), BufferOptions{
		BufferTime: config.Video.MaxBufferTime,
	})
	w.maxBandwidth = config.MaxBandwidth * 1000

	for _, feedback := range w.track.Codec().RTCPFeedback {
		switch feedback.Type {
		case webrtc.TypeRTCPFBTransportCC:
			log.Debugf("Setting feedback %s", webrtc.TypeRTCPFBTransportCC)
			w.feedback = webrtc.TypeRTCPFBTransportCC
			w.wg.Add(1)
			//transport-cc策略
			go w.tccLoop(config.Video.TCCCycle)
		case webrtc.TypeRTCPFBGoogREMB:
			log.Debugf("Setting feedback %s", webrtc.TypeRTCPFBGoogREMB)
			w.feedback = webrtc.TypeRTCPFBGoogREMB
			w.wg.Add(1)
			//丢包统计策略REMB
			go w.rembLoop(config.Video.REMBCycle)
		}
	}
	if w.Track().RID() != "" {
		w.wg.Add(1)
		go w.rembLoop(config.Video.REMBCycle)
	}
	// Start rtcp reader from track
	w.wg.Add(1)
	//接收rtp包，统计transport-cc
	go w.receiveRTP()
	// Start buffer loop
	w.wg.Add(1)
	//buffer统计的rctp发送，如nack，每一组包统计一次
	go w.bufferRtcpLoop()
	// Receiver start loops done, send start signal
	//发送给所有订阅者
	go w.fwdRTP()
	wStart <- struct{}{}
	w.wg.Wait()
}

func startAudioReceiver(w *WebRTCReceiver, wStart chan struct{}) {
	defer func() {
		close(w.rtpCh)
		if w.onCloseHandler != nil {
			w.onCloseHandler()
		}
	}()
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		for {
			pkt, err := w.track.ReadRTP()
			// EOF signal received, this means that the remote track has been removed
			// or the peer has been disconnected. The router must be gracefully shutdown
			if err == io.EOF {
				w.Close()
				return
			}

			if err != nil {
				log.Errorf("rtp err => %v", err)
				continue
			}

			select {
			case <-w.ctx.Done():
				return
			default:
				w.rtpCh <- pkt
			}
		}
	}()
	//音频接收较简单，直接转发即可
	go w.fwdRTP()
	wStart <- struct{}{}
	w.wg.Wait()
}
