package sfu

import (
	log "github.com/pion/ion-log"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
)

type queue struct {
	pkts     []*rtp.Packet
	ssrc     uint32
	head     int //buffer中插入的最新位置，todo: head采用递减方式 -> q.head = (q.head - 1) & (len(q.pkts) - 1) -> 见test
	tail     int //buffer中插入的最早位置，todo: tail采用递减方式 -> q.tail = (q.tail - 1) & (len(q.pkts) - 1) -> 见test
	size     int
	headSN   uint16 //当前最新插入的sequenceNumber
	counter  int
	duration uint32
	onLost   func(nack *rtcp.TransportLayerNack)
}

func (q *queue) AddPacket(pkt *rtp.Packet, latest bool) {
	if !latest {
		q.set(int(q.headSN-pkt.SequenceNumber), pkt)
		return
	}
	diff := pkt.SequenceNumber - q.headSN
	q.headSN = pkt.SequenceNumber
	for i := uint16(1); i < diff; i++ {
		q.push(nil)
		q.counter++
	}
	q.counter++
	q.push(pkt)
	//每7个pkt，生成一次nack，其覆盖连续5个pkt
	if q.counter >= 7 {
		if n := q.nack(); n != nil && q.onLost != nil {
			q.onLost(&rtcp.TransportLayerNack{
				MediaSSRC: q.ssrc,
				Nacks:     []rtcp.NackPair{*n},
			})
		}
		q.clean()
		q.counter -= 5
	}
}

func (q *queue) GetPacket(sn uint16) *rtp.Packet {
	//计算需要的sn与headSN的diff，然后映射到head位置
	return q.get(int(q.headSN - sn))
}

func (q *queue) push(pkt *rtp.Packet) {
	q.resize()
	//todo: head会loop递减，到达0后回到最大值
	q.head = (q.head - 1) & (len(q.pkts) - 1) //todo: 解读
	q.pkts[q.head] = pkt
	q.size++
}

//清理tail位置的pkt，即最早的pkt
func (q *queue) shift() {
	if q.size <= 0 {
		return
	}
	//todo: tail会loop递减，到达0后回到最大值
	q.tail = (q.tail - 1) & (len(q.pkts) - 1) //todo: 解读
	q.pkts[q.tail] = nil
	q.size--
}

//找到tail位置
func (q *queue) last() *rtp.Packet {
	return q.pkts[(q.tail-1)&(len(q.pkts)-1)]
}

func (q *queue) get(i int) *rtp.Packet {
	if i < 0 || i >= q.size {
		return nil
	}
	//从head位置加上sn diff，即为需要获取的位置
	return q.pkts[(q.head+i)&(len(q.pkts)-1)]
}

func (q *queue) set(i int, pkt *rtp.Packet) {
	if i < 0 || i >= q.size {
		log.Warnf("warn: %v:", errPacketTooOld)
		return
	}
	q.pkts[(q.head+i)&(len(q.pkts)-1)] = pkt
}

func (q *queue) resize() {
	//首次分配128大小
	if len(q.pkts) == 0 {
		q.pkts = make([]*rtp.Packet, 128)
		return
	}
	//使用溢出后，分配原先2倍的大小buffer
	if q.size == len(q.pkts) {
		newBuf := make([]*rtp.Packet, q.size<<1)
		//如果tail>head，即不存在尾接，直接拷贝至新buffer
		if q.tail > q.head {
			copy(newBuf, q.pkts[q.head:q.tail])
		} else { //否则存在尾接，先拷贝[head:最后]，再拷贝[开始:tail]
			n := copy(newBuf, q.pkts[q.head:])
			copy(newBuf[n:], q.pkts[:q.tail])
		}
		q.head = 0
		q.tail = q.size
		q.pkts = newBuf
	}
}

//每5个pkt生成一个nack
func (q *queue) nack() *rtcp.NackPair {
	for i := 0; i < 5; i++ {
		if q.get(q.counter-i-1) == nil {
			blp := uint16(0)
			for j := 1; j < q.counter-i; j++ {
				if q.get(q.counter-i-j-1) == nil {
					blp |= 1 << (j - 1)
				}
			}
			return &rtcp.NackPair{PacketID: q.headSN - uint16(q.counter-i-1), LostPackets: rtcp.PacketBitmap(blp)}
		}
	}
	return nil
}

//每隔120个pkt 或者 达到duration间隔，就清理一次buffer
func (q *queue) clean() {
	last := q.last()
	for q.size > 120 && (last == nil || q.pkts[q.head].Timestamp-last.Timestamp > q.duration) {
		q.shift()
	}
}
