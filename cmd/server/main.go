// Command server runs the migration dependency planner web service.
package main

import (
	"flag"
	"log"
	"net/http"
	"time"

	"migrationplanner/internal/model"
	"migrationplanner/internal/store"
	"migrationplanner/internal/web"
)

func seed(s *store.Store) {
	if len(s.Events()) > 0 {
		return
	}
	now := time.Now()
	nodes := []model.Node{
		{
			ID: "create-index", Name: "创建新索引",
			MutexResources:    []string{"table-orders"},
			Duration:          15 * time.Minute,
			Rollbackable:      true,
			OutputFingerprint: "fp-index",
			UpdatedAt:         now,
		},
		{
			ID: "backfill", Name: "回填历史数据",
			DependsOn:         []string{"create-index"},
			MutexResources:    []string{"table-orders"},
			Duration:          40 * time.Minute,
			Rollbackable:      true,
			InputFingerprint:  "fp-index",
			OutputFingerprint: "fp-backfilled",
			UpdatedAt:         now,
		},
		{
			ID: "switch-reads", Name: "读流量切到新索引",
			DependsOn:         []string{"backfill"},
			Duration:          10 * time.Minute,
			Rollbackable:      true,
			InputFingerprint:  "fp-backfilled",
			OutputFingerprint: "fp-reads-switched",
			UpdatedAt:         now,
		},
		{
			ID: "drop-old-index", Name: "删除旧索引（不可逆）",
			DependsOn:         []string{"switch-reads"},
			MutexResources:    []string{"table-orders"},
			Duration:          10 * time.Minute,
			Rollbackable:      false,
			InputFingerprint:  "fp-reads-switched",
			OutputFingerprint: "fp-final",
			UpdatedAt:         now,
		},
	}
	for i := range nodes {
		n := nodes[i]
		if _, _, err := s.Append(model.Event{Type: model.EvNodeUpserted, Node: &n}, "seed-"+n.ID); err != nil {
			log.Fatalf("播种演示数据失败: %v", err)
		}
	}
	log.Printf("已播种 %d 个演示迁移节点", len(nodes))
}

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "监听地址")
	dataDir := flag.String("data", "data", "事件日志目录")
	flag.Parse()

	s, err := store.Open(*dataDir)
	if err != nil {
		log.Fatalf("打开存储失败: %v", err)
	}
	seed(s)

	srv := web.NewServer(s)
	log.Printf("迁移依赖规划器已启动: http://%s", *addr)
	if err := http.ListenAndServe(*addr, srv.Routes()); err != nil {
		log.Fatal(err)
	}
}
