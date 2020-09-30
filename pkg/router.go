package sfu

import (
	"math/rand"
	"sync"
	"time"

	"github.com/pion/ion-sfu/pkg/log"
)

const (
	SimpleRouter = iota + 1
	SimulcastRouter
	SVCRouter
)

// Router defines a track rtp/rtcp router
type Router interface {
	ID() string
	AddReceiver(recv Receiver)
	GetReceiver(layer uint8) Receiver
	AddSender(p *WebRTCTransport) error
	SwitchSpatialLayer(currentLayer, targetLayer uint8, sub Sender) bool
}

// RouterConfig defines router configurations
type RouterConfig struct {
	REMBFeedback bool                      `mapstructure:"subrembfeedback"`
	MaxBandwidth uint64                    `mapstructure:"maxbandwidth"`
	MaxNackTime  int64                     `mapstructure:"maxnacktime"`
	Video        WebRTCVideoReceiverConfig `mapstructure:"video"`
	Simulcast    SimulcastConfig           `mapstructure:"simulcast"`
}

type router struct {
	tid       string
	mu        sync.RWMutex
	kind      int
	config    RouterConfig
	receivers [3 + 1]Receiver
}

// newRouter for routing rtp/rtcp packets
func newRouter(tid string, config RouterConfig, kind int) Router {
	return &router{
		tid:    tid,
		config: config,
		kind:   kind,
	}
}

func (r *router) ID() string {
	return r.tid
}

func (r *router) AddReceiver(recv Receiver) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.receivers[recv.SpatialLayer()] = recv
}

func (r *router) GetReceiver(layer uint8) Receiver {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.receivers[layer]
}

// AddWebRTCSender to router
func (r *router) AddSender(p *WebRTCTransport) error {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var (
		recv   Receiver
		sender Sender
		ssrc   uint32
	)
	//如果是 SimpleRouter 模式，则直接选择第一个pub流进行订阅
	if r.kind == SimpleRouter {
		recv = r.receivers[0]
		ssrc = recv.Track().SSRC()
	} else {//todo: 如果是 SimulcastRouter 模式，此处并没有选择最佳流的逻辑实现？？？
		for _, rcv := range r.receivers {
			recv = rcv
			if !r.config.Simulcast.BestQualityFirst && rcv != nil {
				break
			}
		}
		ssrc = rand.Uint32()
	}

	if recv == nil {
		return errNoReceiverFound
	}

	inTrack := recv.Track()
	to := p.me.GetCodecsByName(recv.Track().Codec().Name)
	if len(to) == 0 {
		return errPtNotSupported
	}
	pt := to[0].PayloadType
	label := inTrack.Label()
	// Simulcast omits stream id, use transport label to keep all tracks under same stream
	//todo: SimulcastRouter会忽略streamID？？？
	if r.kind == SimulcastRouter {
		label = p.label
	}
	outTrack, err := p.pc.NewTrack(pt, ssrc, inTrack.ID(), label)
	if err != nil {
		return err
	}
	// Create webrtc sender for the peer we are sending track to
	s, err := p.pc.AddTrack(outTrack)
	if err != nil {
		return err
	}
	if r.kind == SimulcastRouter {
		sender = NewWebRTCSimulcastSender(p.ctx, p.id, r, s, recv.SpatialLayer())
	} else {
		sender = NewWebRTCSender(p.ctx, p.id, r, s)
	}
	sender.OnCloseHandler(func() {
		if err := p.pc.RemoveTrack(s); err != nil {
			log.Errorf("Error closing sender: %s", err)
		}
	})
	go func() {
		// There exists a bug in chrome where setLocalDescription
		// fails if track RTP arrives before the sfu offer is set.
		// We delay sending RTP here to avoid the issue.
		// https://bugs.chromium.org/p/webrtc/issues/detail?id=10139
		time.Sleep(500 * time.Millisecond)
		//将sender添加到对应的receiver，即订阅该receiver
		recv.AddSender(sender)
	}()
	return nil
}

func (r *router) SwitchSpatialLayer(currentLayer, targetLayer uint8, sub Sender) bool {
	currentRecv := r.GetReceiver(currentLayer)
	targetRecv := r.GetReceiver(targetLayer)
	if targetRecv != nil {
		// TODO do a more smart layer change
		currentRecv.DeleteSender(sub.ID())
		targetRecv.AddSender(sub)
		return true
	}
	return false
}

func (r *router) stats() string {
	return ""
}
