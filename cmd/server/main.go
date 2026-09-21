package main

import (
	"flag"
	"log"
	"net/http"

	"migplanner/internal/model"
	"migplanner/internal/store"
	"migplanner/internal/web"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:5222", "HTTP 监听地址")
	dataDir := flag.String("data", "data", "事件日志数据目录")
	flag.Parse()

	st, err := store.Open(*dataDir)
	if err != nil {
		log.Fatalf("打开存储失败: %v", err)
	}
	defer st.Close()

	if len(st.ListNodes()) == 0 {
		seed(st)
		log.Printf("已写入演示节点定义（首次启动）")
	}

	srv := web.New(st)
	log.Printf("迁移依赖规划器已启动: http://%s", *addr)
	log.Printf("数据文件: %s", st.DataFile())
	if err := http.ListenAndServe(*addr, srv.Handler()); err != nil {
		log.Fatal(err)
	}
}

func seed(st *store.Store) {
	nodes := []*model.Node{
		{ID: "M01", Name: "在线加索引（后台）", Prereqs: nil, Resources: nil, Minutes: 20,
			Rollback: true, Inputs: []string{"fp-v3"}, Output: "fp-v4-a"},
		{ID: "M02", Name: "回填历史数据", Prereqs: []string{"M01"}, Resources: nil, Minutes: 30,
			Rollback: true, Inputs: []string{"fp-v4-a"}, Output: "fp-v4-b"},
		{ID: "M03", Name: "双写新表", Prereqs: []string{"M01"}, Resources: []string{"lock-meta"},
			Minutes: 25, Rollback: true, Inputs: []string{"fp-v4-a"}, Output: "fp-v4-c"},
		{ID: "M04", Name: "元数据切换（不可回滚）", Prereqs: []string{"M02", "M03"},
			Resources: []string{"lock-meta"}, Minutes: 10, Rollback: false,
			Inputs: []string{"fp-v4-b", "fp-v4-c"}, Output: "fp-v5"},
		{ID: "M05", Name: "校验与清理", Prereqs: []string{"M04"}, Resources: nil, Minutes: 15,
			Rollback: true, Inputs: []string{"fp-v5"}, Output: "fp-v5-clean"},
		{ID: "M06", Name: "不兼容起始指纹的任务", Prereqs: nil, Resources: nil, Minutes: 10,
			Rollback: true, Inputs: []string{"fp-legacy"}, Output: "fp-x"},
	}
	for _, n := range nodes {
		if err := st.SaveNode(n); err != nil {
			log.Printf("种子节点 %s 写入失败: %v", n.ID, err)
		}
	}
}
