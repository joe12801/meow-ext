package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// 多选音源时必须轮转分配名额：早先按 sources 顺序摊平后截断，
// 排在前面的音源会把名额吃满。设备上 sources 是按字母序保存的
// （bilibili 排第一），结果就是"只有 B 站和 5sing"，而日志还写着
// "8/9 个音源有结果"——这个测试就是钉住这个回归。
func TestMeowSearchRoundRobinsAcrossSources(t *testing.T) {
	// 每个 source 都返回 10 条各自的曲目，总计 3 个源。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		src := r.URL.Query().Get("source")
		out := make([]map[string]any, 0, 10)
		for i := 0; i < 10; i++ {
			out = append(out, map[string]any{
				"title":  fmt.Sprintf("%s-%02d", src, i),
				"artist": src,
				"source": src,
				"url":    "http://meow.local/stream_live?song=x&singer=y",
			})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	}))
	defer srv.Close()

	c := NewMeowClient(Config{MeowURL: srv.URL})
	got, err := c.Search(context.Background(), "测试", 9, []string{"bilibili", "fivesing", "kugou"})
	if err != nil {
		t.Fatalf("Search 出错：%v", err)
	}
	if len(got) != 9 {
		t.Fatalf("应返回 9 条（受 limit 限制），实际 %d 条", len(got))
	}
	per := map[string]int{}
	for _, tr := range got {
		per[tr.Source]++
	}
	// 9 条均分到 3 个源：每个源都必须是 3 条，不能出现某个源 0 条。
	for _, src := range []string{"bilibili", "fivesing", "kugou"} {
		if per[src] != 3 {
			t.Fatalf("音源 %s 应有 3 条，实际分布 %v", src, per)
		}
	}
	// 轮转顺序：第一条来自第一个源，第二条来自第二个源。
	if got[0].Source != "bilibili" || got[1].Source != "fivesing" || got[2].Source != "kugou" {
		t.Fatalf("未按轮转顺序摊平：%s %s %s", got[0].Source, got[1].Source, got[2].Source)
	}
}

// 单个音源仍然逐字用它自己的 limit，不做均分（否则单选音源会只拿到几条）。
func TestMeowSearchSingleSourceKeepsLimit(t *testing.T) {
	var seenLimit string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenLimit = r.URL.Query().Get("limit")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	c := NewMeowClient(Config{MeowURL: srv.URL})
	if _, err := c.Search(context.Background(), "测试", 30, []string{"kuwo"}); err != nil {
		t.Fatalf("Search 出错：%v", err)
	}
	if seenLimit != "30" {
		t.Fatalf("单音源应透传 limit=30，实际 %q", seenLimit)
	}
}
