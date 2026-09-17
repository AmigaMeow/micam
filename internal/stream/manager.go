package stream

import (
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/flv"
	"github.com/AlexxIT/go2rtc/pkg/mpegts"
	"github.com/AlexxIT/go2rtc/pkg/xiaomi"
)

// Format 输出格式。
type Format string

const (
	FormatFLV    Format = "flv"
	FormatMPEGTS Format = "mpegts"
)

// Stream 是一条已建立的流：一个 Producer + 多个 Consumer。
type Stream struct {
	Name      string
	Source    string
	mu        sync.Mutex
	producer  core.Producer
	consumers []core.Consumer
	started   bool
}

// Manager 管理所有流。
type Manager struct {
	mu      sync.Mutex
	streams map[string]*Stream
}

func NewManager() *Manager {
	return &Manager{streams: map[string]*Stream{}}
}

// SourceURL 拼 go2rtc 的 xiaomi:// 源地址。
//   subtype=3 → 2K（默认档位 2 只有 640x360）
//   channel=2 → 双摄第二镜头
//   vendor=cs2 → 主流机型
func SourceURL(did, model, ip, userID, region string, opts ...string) string {
	u := fmt.Sprintf("xiaomi://%s:%s@%s?did=%s&model=%s&subtype=3",
		userID, region, ip, did, model)
	for _, o := range opts {
		u += "&" + o
	}
	return u
}

// Get 取一条流（不存在则创建，但尚未 Dial）。
func (m *Manager) Get(name, source string) *Stream {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.streams[name]; ok {
		return s
	}
	s := &Stream{Name: name, Source: source}
	m.streams[name] = s
	return s
}

// Remove 删除。
func (m *Manager) Remove(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.streams[name]; ok {
		s.Close()
		delete(m.streams, name)
	}
}

// List 列出所有流名。
func (m *Manager) List() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.streams))
	for k := range m.streams {
		out = append(out, k)
	}
	return out
}

// ---- Stream ----

// ensureProducer 建立 producer 连接（幂等）。
func (s *Stream) ensureProducer() (core.Producer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.producer != nil {
		return s.producer, nil
	}
	prod, err := xiaomi.Dial(s.Source)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", s.Name, err)
	}
	if err := prod.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", s.Name, err)
	}
	s.producer = prod
	return prod, nil
}

// AddConsumer 把 producer 的轨道绑到 consumer 上。
func (s *Stream) AddConsumer(cons core.Consumer) error {
	prod, err := s.ensureProducer()
	if err != nil {
		return err
	}

	consMedias := cons.GetMedias()
	for _, prodMedia := range prod.GetMedias() {
		for _, consMedia := range consMedias {
			prodCodec, consCodec := prodMedia.MatchMedia(consMedia)
			if prodCodec == nil {
				continue
			}
			if prodMedia.Direction != core.DirectionRecvonly {
				continue
			}
			track, err := prod.GetTrack(prodMedia, prodCodec)
			if err != nil {
				continue
			}
			if err := cons.AddTrack(consMedia, consCodec, track); err != nil {
				return err
			}
		}
	}

	s.mu.Lock()
	s.consumers = append(s.consumers, cons)
	s.mu.Unlock()

	return cons.(interface{ Start() error }).Start()
}

// RemoveConsumer 移除 consumer。
func (s *Stream) RemoveConsumer(cons core.Consumer) {
	s.mu.Lock()
	for i, c := range s.consumers {
		if c == cons {
			s.consumers = append(s.consumers[:i], s.consumers[i+1:]...)
			break
		}
	}
	s.mu.Unlock()
	if err := cons.Stop(); err != nil {
		log.Printf("[stream] 停止 consumer 失败: %v", err)
	}
}

// Close 关闭整条流。
func (s *Stream) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.producer != nil {
		if err := s.producer.Stop(); err != nil {
			log.Printf("[stream] 停止 producer 失败: %v", err)
		}
		s.producer = nil
	}
	for _, c := range s.consumers {
		if err := c.Stop(); err != nil {
			log.Printf("[stream] 停止 consumer 失败: %v", err)
		}
	}
	s.consumers = nil
}

// Info 流信息。
type Info struct {
	Name    string   `json:"name"`
	Source  string   `json:"source"`
	Video   string   `json:"video,omitempty"`
	Audio   string   `json:"audio,omitempty"`
	HasProd bool     `json:"hasProducer"`
	Tracks  []string `json:"tracks,omitempty"`
}

// Info 返回当前流的媒体信息（会触发一次 Dial）。
func (s *Stream) Info() (*Info, error) {
	prod, err := s.ensureProducer()
	if err != nil {
		return nil, err
	}
	info := &Info{Name: s.Name, Source: s.Source, HasProd: true}
	for _, media := range prod.GetMedias() {
		for _, codec := range media.Codecs {
			line := fmt.Sprintf("%s/%d", codec.Name, codec.ClockRate)
			info.Tracks = append(info.Tracks, media.Kind+":"+line)
			if media.Kind == "video" {
				info.Video = line
			} else if info.Audio == "" {
				info.Audio = line
			}
		}
	}
	return info, nil
}

// ReadBytes 读取若干字节，验证流真的有数据。
func (s *Stream) ReadBytes(cons core.Consumer, n int) ([]byte, error) {
	type reader interface{ Read([]byte) (int, error) }
	r, ok := cons.(reader)
	if !ok {
		return nil, fmt.Errorf("consumer 不支持 Read")
	}
	buf := make([]byte, n)
	total := 0
	deadline := time.Now().Add(15 * time.Second)
	for total < n && time.Now().Before(deadline) {
		m, err := r.Read(buf[total:])
		if m > 0 {
			total += m
		}
		if err != nil {
			if total > 0 {
				break
			}
			return nil, err
		}
	}
	if total == 0 {
		return nil, fmt.Errorf("超时未读到数据")
	}
	return buf[:total], nil
}

// NewFLV 创建 FLV consumer。
func NewFLV() *flv.Consumer { return flv.NewConsumer() }

// NewTS 创建 MPEG-TS consumer。
func NewTS() *mpegts.Consumer { return mpegts.NewConsumer() }

// MarshalJSON 导出所有流。
func (m *Manager) MarshalJSON() ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []*Info{}
	for _, s := range m.streams {
		out = append(out, &Info{Name: s.Name, Source: s.Source})
	}
	return json.Marshal(out)
}
