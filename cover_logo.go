package main

import (
	"embed"
	"net/http"
	"strconv"
	"strings"
)

// 音源 LOGO 以 PNG 编进二进制。
//
// 为什么不做成运行时读文件：FPK 安装后只有 bin/ 与 ui/ 落地，多带一个资源目录
// 就要在打包、权限、升级三处各维护一份路径；而这几张图一共一百多 KB，
// embed 进去最不容易坏。
//
//go:embed assets/logos/*.png
var logoFS embed.FS

// coverIDPrefixes 是飞牛前端给封面 id 加的类目前缀。
// 前端拿到 track.coverId 后会拼成 "track_<id>" 再请求 /static/cover，
// 所以同一首曲子在不同位置可能带不同前缀，匹配前统统一层层剥掉。
var coverIDPrefixes = []string{
	"track_", "album_", "artist_", "playlist_", "folder_", "cover_", "cue_",
}

// logoAliases 把上游可能出现的别名归一到文件名。
// 只做"叫法统一"，不做白名单：认不出的源会走 meow.png 兜底，
// 不能让上游新增音源就变成一张灰色音符。
var logoAliases = map[string]string{
	"netease_cloud": "netease",
	"neteasemusic":  "netease",
	"wy":            "netease",
	"qqmusic":       "qq",
	"bili":          "bilibili",
	"xmly":          "ximalaya",
	"5sing":         "fivesing",
	"qianqian":      "kuwo",
	"migu":          "kuwo",
}

// logoKeys 与 assets/logos 下的文件名保持一致。
var logoKeys = map[string]struct{}{
	"kuwo": {}, "netease": {}, "kugou": {}, "qq": {}, "ximalaya": {},
	"fivesing": {}, "lizhi": {}, "bilibili": {}, "qishui": {}, "ext": {},
	"youtube": {}, "meow": {},
}

// coverIDParam 取封面 id。飞牛有两种传法：查询参数 coverId=，
// 以及新版路径式 /static/cover/<coverId>。
func coverIDParam(r *http.Request) string {
	q := r.URL.Query()
	// 先看 url= 系列是否已经带了真实封面地址；带的话走代理，不该盖成语义化 LOGO。
	for _, k := range []string{"coverId", "coverID", "cover_id"} {
		if v := strings.TrimSpace(q.Get(k)); v != "" {
			return v
		}
	}
	if v := tailSegment(r.URL.Path, "/music/api/v1/static/cover/"); v != "" {
		return v
	}
	return ""
}

func stripCoverIDPrefixes(id string) string {
	for {
		low := strings.ToLower(id)
		stripped := false
		for _, p := range coverIDPrefixes {
			if strings.HasPrefix(low, p) && len(id) > len(p) {
				id = id[len(p):]
				stripped = true
				break
			}
		}
		if !stripped {
			return id
		}
	}
}

// serveSourceLogo 输出音源 LOGO（见下方注释）。
//
// 缓存刻意给得很短（60 秒）：它是"海报还没解析出来"的占位图，
// 一旦标成 immutable 一周，客户端就会把 LOGO 钉死在这个 URL 上，
// 之后即使海报已经就绪也换不回来——正是"只显示音源 LOGO"的成因。
func serveSourceLogo(w http.ResponseWriter, r *http.Request, source string) {
	png := sourceLogo(source)
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Content-Length", strconv.Itoa(len(png)))
	w.Header().Set("Cache-Control", logoCacheControl)
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		w.Write(png)
	}
}

func sourceLogo(source string) []byte {
	if b, err := logoFS.ReadFile("assets/logos/" + normalizeSource(source) + ".png"); err == nil {
		return b
	}
	b, _ := logoFS.ReadFile("assets/logos/meow.png")
	return b
}

func normalizeSource(source string) string {
	k := strings.ToLower(strings.TrimSpace(source))
	if a, ok := logoAliases[k]; ok {
		k = a
	}
	if _, ok := logoKeys[k]; ok {
		return k
	}
	return "meow"
}
