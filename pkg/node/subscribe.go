package sfu

import (
	"errors"

	"github.com/lucsky/cuid"
	"github.com/pion/ion-sfu/pkg/log"
	"github.com/pion/ion-sfu/pkg/media"
	"github.com/pion/ion-sfu/pkg/rtc"
	transport "github.com/pion/ion-sfu/pkg/rtc/transport"
	"github.com/pion/webrtc/v2"
)

// Subscribe to a mid
func Subscribe(mid string, offer webrtc.SessionDescription) (string, *webrtc.PeerConnection, *webrtc.SessionDescription, error) {
	me := media.Engine{}
	if err := me.PopulateFromSDP(offer); err != nil {
		return "", nil, nil, errSdpParseFailed
	}

	router := rtc.GetRouter(mid)

	if router == nil {
		return "", nil, nil, errRouterNotFound
	}

	pub := router.GetPub().(*transport.WebRTCTransport)

	//sub端根据媒体协商结果，构造mapping结构，用于保存payloadType映射
	me.MapFromEngine(pub.MediaEngine())

	api := webrtc.NewAPI(webrtc.WithMediaEngine(me.MediaEngine), webrtc.WithSettingEngine(setting))
	pc, err := api.NewPeerConnection(cfg)

	if err != nil {
		log.Errorf("Subscribe error: %v", err)
		return "", nil, nil, errPeerConnectionInitFailed
	}

	sub := transport.NewWebRTCTransport(cuid.New(), pc, &me)

	if sub == nil {
		return "", nil, nil, errors.New("Subscribe: transport.NewWebRTCTransport failed")
	}

	pc.OnICEConnectionStateChange(func(connectionState webrtc.ICEConnectionState) {
		switch connectionState {
		case webrtc.ICEConnectionStateDisconnected:
			log.Infof("webrtc ice disconnected for mid: %s", mid)
		case webrtc.ICEConnectionStateFailed:
			fallthrough
		case webrtc.ICEConnectionStateClosed:
			log.Infof("webrtc ice closed for mid: %s", mid)
			sub.Close()
		}
	})

	// Add existing pub tracks to sub
	//在生成answer之前，添加track
	//根据inTrack的payload和ssrc，生成outTrack的ssrc
	//payload根据mapping映射得到(sfu只做转发，其媒体能力根据端上自适应，由于pub端和sub端的媒体能力不一致，所以在转发时需要进行payload映射)
	//ssrc则保持inTrack和outTrack一致
	//那么生成answer时，就会根据该payload和ssrc生成对应的MediaDescription
	for ssrc, track := range pub.GetInTracks() {
		log.Debugf("AddTrack: codec:%s, ssrc:%d, streamID %s, trackID %s", track.Codec().MimeType, ssrc, mid, track.ID())
		_, err := sub.AddOutTrack(mid, track)
		if err != nil {
			log.Errorf("err=%v", err)
		}
	}

	err = pc.SetRemoteDescription(offer)
	if err != nil {
		log.Errorf("Subscribe error: pc.SetRemoteDescription %v", err)
		return "", nil, nil, err
	}

	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		log.Errorf("Subscribe error: pc.CreateAnswer answer=%v err=%v", answer, err)
		return "", nil, nil, err
	}

	err = pc.SetLocalDescription(answer)
	if err != nil {
		log.Errorf("Subscribe error: pc.SetLocalDescription answer=%v err=%v", answer, err)
		return "", nil, nil, err
	}

	router.AddSub(sub.ID(), sub)

	log.Debugf("Subscribe: mid %s, answer = %v", sub.ID(), answer)
	return sub.ID(), pc, &answer, nil
}
