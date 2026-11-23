package main

import (
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// guidPrefix 是扩展下发给飞牛客户端的在线曲目标识前缀。
// 中间那段是音源名（这里固定 meow），与飞牛生态里既有的 online:<源>: 习惯保持一致。
const guidPrefix = "online:meow:"

// _sep 分隔 guid 载荷里的各字段。用 0x1f（单元分隔符）而不是 ':'，
// 因为歌名里出现冒号是家常便饭（"晴天 (Live 版)" 还算好的），
// 而控制字符在曲目元数据里几乎不可能出现。
const _sep = "\x1f"

// MeowRef 是 guid 承载的全部取流信息。
//
// 为什么要把这些塞进 id 而不是只存歌名：Meow 的 /stream_live 支持按 xid / bv
// 直取，喜马拉雅（xid=trackId）、5sing（xid=fs:<songId>:<type>）、荔枝、
// B 站（bv=BV1…）都只能靠 id 定位。丢了它们就只能按"歌名+歌手"重新解析，
// 而同名的播客/翻唱会被解析成另一首歌——用户点的是这集有声书，响的却是同名的歌。
type MeowRef struct {
	Song   string `json:"song"`
	Singer string `json:"singer"`
	XID    string `json:"xid,omitempty"`
	BV     string `json:"bv,omitempty"`
	// Source 是来源音源键名（kuwo/ximalaya/bilibili…），只用于搜索结果里的来源标记。
	Source string `json:"source,omitempty"`
	// Path 是上游给的完整相对播放地址，仅在它不是 /stream_live 时使用
	// （外部源的 /api/ext_audio?… 带签名参数，拆不开也不该拆）。
	// 非空时优先于 Song/Singer/XID/BV 拼出来的地址。
	Path string `json:"path,omitempty"`
}

// EncodeGUID 把取流信息编码成 online:meow:<base64url>。
// base64url 不含 ':' 与 '/'，因此可以安全地出现在 URL 路径与查询串里。
func EncodeGUID(ref MeowRef) string {
	fields := []string{ref.Song, ref.Singer, ref.XID, ref.BV, ref.Source, ref.Path}
	// 从尾部裁掉空字段，能让常见情况（只有歌名+歌手）的 id 短一截。
	// 中间的字段即使为空也保留：位置就是语义，空串代表"没有这一项"。
	for len(fields) > 2 && fields[len(fields)-1] == "" {
		fields = fields[:len(fields)-1]
	}
	raw := strings.Join(fields, _sep)
	return guidPrefix + base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// DecodeGUID 解出取流信息。第二返回值表示"这是一个本扩展的在线 id"，
// 而不是解码成功与否——解不开的一律当作非本扩展 id，交给官方透传。
func DecodeGUID(guid string) (MeowRef, bool) {
	if !strings.HasPrefix(guid, guidPrefix) {
		return MeowRef{}, false
	}
	payload := guid[len(guidPrefix):]
	// 容忍带 padding 的变体：不同版本的客户端可能会重新编码 id。
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(payload, "="))
	if err != nil {
		if raw, err = base64.URLEncoding.DecodeString(payload); err != nil {
			return MeowRef{}, false
		}
	}
	fields := strings.Split(string(raw), _sep)
	if len(fields) < 2 {
		return MeowRef{}, false
	}
	ref := MeowRef{Song: fields[0], Singer: fields[1]}
	if len(fields) > 2 {
		ref.XID = fields[2]
	}
	if len(fields) > 3 {
		ref.BV = fields[3]
	}
	if len(fields) > 4 {
		ref.Source = fields[4]
	}
	if len(fields) > 5 {
		ref.Path = fields[5]
	}
	if strings.TrimSpace(ref.Song) == "" {
		return MeowRef{}, false
	}
	return ref, true
}

// IsMeowGUID 判断是否本扩展的在线 id（含已伪装的 32-hex 形态）。
func (r *GUIDRegistry) IsMeowGUID(id string) bool {
	if strings.HasPrefix(id, guidPrefix) {
		return true
	}
	_, ok := r.Resolve(id)
	return ok
}

// ---- 32-hex 伪装 ----

// GUIDRegistry 维护"真实 id ↔ 32 位十六进制假 id"的双向映射。
//
// 为什么必须伪装：飞牛 App 会按 id 形态过滤条目，非 32-hex 的 online: 前缀 id
// 会被整条丢弃（症状是收藏、播放历史列表一片空白）。因此下发给客户端的每个
// 在线 id 都要换成 32-hex，同时把映射记下来，等客户端拿这个 id 回来取流时再解回。
//
// 映射必须落盘：进程重启后客户端手里仍握着旧 id（用户没刷新页面、App 没重开），
// 内存表一丢，这些点播就全变成 404。这是"重启后点历史里的歌就不响了"的根因。
type GUIDRegistry struct {
	mu sync.RWMutex
	// forward: 真实 id → 假 id
	forward map[string]string
	// reverse: 假 id → 真实 id
	reverse map[string]string
	// order 记录写入顺序，用于按容量淘汰最旧的映射。
	order []string
	path  string
	dirty bool

	// maxEntries 限制映射规模。一首歌一条，两万条足够覆盖一年的收听量，
	// 超出后淘汰最旧的——被淘汰的曲目再搜一次就会重新登记，代价可接受。
	maxEntries int
}

// NewGUIDRegistry 创建空注册表；path 为空表示纯内存（测试用）。
func NewGUIDRegistry(path string) *GUIDRegistry {
	return &GUIDRegistry{
		forward:    make(map[string]string),
		reverse:    make(map[string]string),
		path:       path,
		maxEntries: 20000,
	}
}

// Fake 返回真实 id 对应的 32-hex 假 id，必要时登记新映射。
//
// 用 md5 而不是随机数：同一首歌每次搜索结果必须得到同一个假 id，
// 否则客户端会认为这是两条不同的曲目（收藏重复、列表去重失效）。
func (r *GUIDRegistry) Fake(real string) string {
	if !strings.HasPrefix(real, guidPrefix) {
		return real
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if fake, ok := r.forward[real]; ok {
		return fake
	}
	sum := md5.Sum([]byte("meow-ext::" + real))
	fake := hex.EncodeToString(sum[:])
	// md5 撞在 2^64 量级的碰撞概率下可忽略，但真撞上就会串曲目。
	// 花一次查表确认，把这种不可解释的故障挡在门外。
	if existing, ok := r.reverse[fake]; ok && existing != real {
		for i := 0; ; i++ {
			sum = md5.Sum([]byte("meow-ext::" + real + "#" + string(rune('a'+i%26))))
			fake = hex.EncodeToString(sum[:])
			if _, taken := r.reverse[fake]; !taken {
				break
			}
		}
	}
	r.forward[real] = fake
	r.reverse[fake] = real
	r.order = append(r.order, real)
	r.dirty = true
	r.evictLocked()
	return fake
}

// Resolve 把客户端传来的 id 解回真实 id。
// 同时接受假 id 与原始 online: id，便于排障时直接拿真 id 打接口。
func (r *GUIDRegistry) Resolve(id string) (string, bool) {
	if strings.HasPrefix(id, guidPrefix) {
		return id, true
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	real, ok := r.reverse[id]
	return real, ok
}

func (r *GUIDRegistry) evictLocked() {
	if len(r.order) <= r.maxEntries {
		return
	}
	drop := len(r.order) - r.maxEntries
	for _, real := range r.order[:drop] {
		if fake, ok := r.forward[real]; ok {
			delete(r.forward, real)
			delete(r.reverse, fake)
		}
	}
	// 重新切片而不是原地移位：底层数组会被 GC 回收，避免长期持有已淘汰的字符串。
	r.order = append([]string(nil), r.order[drop:]...)
}

// Load 从磁盘读回映射。文件不存在视为首次运行。
func (r *GUIDRegistry) Load() error {
	if r.path == "" {
		return nil
	}
	data, err := os.ReadFile(r.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var stored map[string]string
	if err := json.Unmarshal(data, &stored); err != nil {
		// 映射表损坏不该让扩展起不来：丢掉它只会让旧 id 失效，
		// 重新搜索即可恢复；而起不来是整个音源都不可用。
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for real, fake := range stored {
		r.forward[real] = fake
		r.reverse[fake] = real
		r.order = append(r.order, real)
	}
	r.evictLocked()
	return nil
}

// Save 落盘。只在有变更时写，避免每条搜索都产生一次磁盘 IO。
func (r *GUIDRegistry) Save() error {
	if r.path == "" {
		return nil
	}
	r.mu.Lock()
	if !r.dirty {
		r.mu.Unlock()
		return nil
	}
	snapshot := make(map[string]string, len(r.forward))
	for k, v := range r.forward {
		snapshot[k] = v
	}
	r.dirty = false
	r.mu.Unlock()

	if err := os.MkdirAll(filepath.Dir(r.path), 0o750); err != nil {
		return err
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(r.path), ".guidmap-*.json")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, r.path)
}

// Size 返回当前映射条数（管理页展示用）。
func (r *GUIDRegistry) Size() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.forward)
}
