// Copyright 2017 Xiaomi, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package sender

import (
	"fmt"
	"log"
	"strconv"
	"sync"
	"time"

	"encoding/json"

	infclient "github.com/influxdata/influxdb/client/v2"
	backend "github.com/open-falcon/falcon-plus/common/backend_pool"
	cmodel "github.com/open-falcon/falcon-plus/common/model"
	"github.com/open-falcon/falcon-plus/common/utils"
	"github.com/open-falcon/falcon-plus/modules/transfer/g"
	"github.com/open-falcon/falcon-plus/modules/transfer/proc"
	"github.com/toolkits/consistent/rings"
	nlist "github.com/toolkits/container/list"
)

const (
	DefaultSendQueueMaxSize = 102400 //10.24w
)

// 默认参数
var (
	MinStep int //最小上报周期,单位sec
)

// 服务节点的一致性哈希环
// pk -> node
var (
	JudgeNodeRing *rings.ConsistentHashNodeRing
	PolyNodeRing  *rings.ConsistentHashNodeRing
	GraphNodeRing *rings.ConsistentHashNodeRing
)

// 发送缓存队列
// node -> queue_of_data
var (
	//KafkaQueues string
	TsdbQueue   *nlist.SafeListLimited
	JudgeQueues = make(map[string]*nlist.SafeListLimited)
	GraphQueues = make(map[string]*nlist.SafeListLimited)
	PolyQueues  = make(map[string]*nlist.SafeListLimited)
)

// 连接池
// node_address -> connection_pool
var (
	JudgeConnPools     *backend.SafeRpcConnPools
	TsdbConnPoolHelper *backend.TsdbConnPoolHelper
	GraphConnPools     *backend.SafeRpcConnPools
	PolyConnPools      *backend.SafeRpcConnPools
)

var (
	KafkaQueue    = make(chan *cmodel.KafkaData, 64000) //设置一个缓存为64000的channel
	InfluxDBQueue = make(chan *cmodel.MetaData, 64000)  //设置一个缓存为64000的channel
)

// 初始化数据发送服务, 在main函数中调用
func Start() { // 初始化默认参数
	MinStep = g.Config().MinStep
	if MinStep < 1 {
		MinStep = 30 //默认30s
	}
	initConnPools() //poly/judge/graph/rrdtool
	initSendQueues()
	initNodeRings()
	// SendTasks依赖基础组件的初始化,要最后启动
	startSendTasks()
	startSenderCron()
	StartKafkaSender()
	//startInfluxDBSender()
	log.Println("send.Start, ok")
}

func startInfluxDBSender() {
	go func() {
		timer := time.NewTicker(g.Config().InfluxDB.Interval * time.Second) //timer控制超时
		defer timer.Stop()
		lock := new(sync.RWMutex)

		ptList := []*infclient.Point{}
		for {
			select {
			case item := <-InfluxDBQueue:
				tags := item.Tags
				tags["Endpoint"] = item.Endpoint
				tags["step"] = strconv.Itoa(int(item.Step))
				fields := map[string]interface{}{
					"Value": item.Value,
				}
				pt, err := infclient.NewPoint(item.Metric, tags, fields, time.Unix(item.Timestamp, 0))
				if err == nil {
					lock.Lock()
					ptList = append(ptList, pt)
					lock.Unlock()
				}
			case <-timer.C:
				conn, err := GetInfluxDbConn()
				if err == nil && conn != nil {
					bp, err := GetNewBatchPoints()
					if err == nil {
						lock.RLock()
						bp.AddPoints(ptList)
						lock.RUnlock()
					}
					if err := conn.Write(bp); err == nil {
						lock.Lock()
						ptList = []*infclient.Point{}
						lock.Unlock()
					}
					conn.Close()
				}

			}
		}
	}()
}

//将缓存数据队列写入kakfa中
func StartKafkaSender() {
	go func() { //读channel的线程
		for {
			items := <-KafkaQueue
			//itemsStr := items.String()
			bs, err := json.Marshal(items)
			if err != nil {
				log.Printf("StartKafkaSender_json.Marshal_err:%+v", err)
				return
			}
			errSend := MetricsBusSend([]byte("falcon"), bs)
			if errSend != nil {
				log.Printf("StartKafkaSender_error:%+v", errSend)
				proc.SendToKafkaFailCnt.IncrBy(1)
			}
		}
	}()
}

// 将数据 打入kafka的发送缓存队列
func Push2KafkaSendQueue(items []*cmodel.MetaData) {
	cnt := int64(len(items))
	proc.SendToInfluxDBCnt.IncrBy(int64(cnt))
	cache := int64(len(KafkaQueue))
	proc.InfluxDBQueuesCnt.SetCnt(int64(cache))
	maps := make(map[string]float64)

	//初始化kafkadata
	kv := &cmodel.KafkaData{
		Endpoint:    "",
		Timestamp:   0,
		MetricValue: maps,
	}

	ExtractKafkaItemNew(items, kv)

	if kv.Endpoint != "" {
		KafkaQueue <- kv
	}

}

func Push2InfluxDBSendQueue(items []*cmodel.MetaData) {
	for _, item := range items {
		InfluxDBQueue <- item
	}
}

func ExtractKafkaItemNew(items []*cmodel.MetaData, kv *cmodel.KafkaData) {
	for _, item := range items {
		// First is index, second is the key element.
		counter := utils.Counter(item.Metric, item.Tags)
		if kv.Endpoint == "" {
			m1 := map[string]float64{
				counter: item.Value,
			}
			kv.Endpoint = item.Endpoint
			kv.Timestamp = item.Timestamp
			kv.MetricValue = m1
		} else {
			kv.MetricValue[counter] = item.Value
		}
	}
}

//将监控的信息聚合输出
//func ExtractKafkaItem(keys []string, items []*cmodel.MetaData, kv *cmodel.KafkaData) {
//	for _, item := range items {
//		// First is index, second is the key element.
//		for _, key := range keys {
//			if item.Metric == key {
//				if len(item.Tags) != 0 {
//					value1, ok1 := item.Tags["device"]
//					if ok1 {
//						key += ".device:"
//						key += value1
//					}
//					value2, ok2 := item.Tags["iface"]
//					if ok2 {
//						key += ".iface:"
//						key += value2
//					}
//				}
//				if kv.Endpoint == "" {
//					m1 := map[string]float64{
//						key: item.Value,
//					}
//					kv.Endpoint = item.Endpoint
//					kv.Timestamp = item.Timestamp
//					kv.MetricValue = m1
//				} else {
//					kv.MetricValue[key] = item.Value
//				}
//
//			}
//		}
//	}
//}

// 将数据 打入 某个Judge的发送缓存队列, 具体是哪一个Judge 由一致性哈希 决定
func Push2JudgeSendQueue(items []*cmodel.MetaData) {
	for _, item := range items {
		pk := item.PK()
		node, err := JudgeNodeRing.GetNode(pk)
		if err != nil {
			log.Println("E:", err)
			continue
		}

		// align ts
		step := int(item.Step)
		if step < MinStep {
			step = MinStep
		}
		ts := alignTs(item.Timestamp, int64(step))

		judgeItem := &cmodel.JudgeItem{
			Endpoint:  item.Endpoint,
			Metric:    item.Metric,
			Value:     item.Value,
			Timestamp: ts,
			JudgeType: item.CounterType,
			Tags:      item.Tags,
		}
		Q := JudgeQueues[node]
		isSuccess := Q.PushFront(judgeItem)

		// statistics
		if !isSuccess {
			proc.SendToJudgeDropCnt.Incr()
		}
	}
}

func Push2PolySendQueue(items []*cmodel.PolyRequest) {

	for _, item := range items {
		//log.Printf("Push2PolySendQueue:%+v", item)
		pk := item.PolyName
		node, err := PolyNodeRing.GetNode(pk)
		if err != nil {
			log.Println("E:", err)
			continue
		}

		Q := PolyQueues[node]
		isSuccess := Q.PushFront(item)

		// statistics
		if !isSuccess {
			proc.SendToPolyDropCnt.Incr()
		}
	}
}

// 将数据 打入 某个Graph的发送缓存队列, 具体是哪一个Graph 由一致性哈希 决定
func Push2GraphSendQueue(items []*cmodel.MetaData) {
	cfg := g.Config().Graph

	for _, item := range items {
		graphItem, err := convert2GraphItem(item)
		if err != nil {
			log.Println("E:", err)
			continue
		}
		pk := item.PK()

		// statistics. 为了效率,放到了这里,因此只有graph是enbale时才能trace
		proc.RecvDataTrace.Trace(pk, item)
		proc.RecvDataFilter.Filter(pk, item.Value, item)

		node, err := GraphNodeRing.GetNode(pk)
		if err != nil {
			log.Println("E:", err)
			continue
		}

		cnode := cfg.ClusterList[node]
		errCnt := 0
		for _, addr := range cnode.Addrs {
			Q := GraphQueues[node+addr]
			if !Q.PushFront(graphItem) {
				errCnt += 1
			}
		}

		// statistics
		if errCnt > 0 {
			proc.SendToGraphDropCnt.Incr()
		}
	}
}

// 打到Graph的数据,要根据rrdtool的特定 来限制 step、counterType、timestamp
func convert2GraphItem(d *cmodel.MetaData) (*cmodel.GraphItem, error) {
	item := &cmodel.GraphItem{}

	item.Endpoint = d.Endpoint
	item.Metric = d.Metric
	item.Tags = d.Tags
	item.Timestamp = d.Timestamp
	item.Value = d.Value
	item.Step = int(d.Step)
	if item.Step < MinStep {
		item.Step = MinStep
	}
	item.Heartbeat = item.Step * 2

	if d.CounterType == g.GAUGE {
		item.DsType = d.CounterType
		item.Min = "U"
		item.Max = "U"
	} else if d.CounterType == g.COUNTER {
		item.DsType = g.DERIVE
		item.Min = "0"
		item.Max = "U"
	} else if d.CounterType == g.DERIVE {
		item.DsType = g.DERIVE
		item.Min = "0"
		item.Max = "U"
	} else {
		return item, fmt.Errorf("not_supported_counter_type")
	}

	item.Timestamp = alignTs(item.Timestamp, int64(item.Step)) //item.Timestamp - item.Timestamp%int64(item.Step)

	return item, nil
}

// 将原始数据入到tsdb发送缓存队列
func Push2TsdbSendQueue(items []*cmodel.MetaData) {
	for _, item := range items {
		tsdbItem := convert2TsdbItem(item)
		log.Println("tsdbItem:", tsdbItem)
		isSuccess := TsdbQueue.PushFront(tsdbItem)

		if !isSuccess {
			proc.SendToTsdbDropCnt.Incr()
		}
	}
}

// 转化为tsdb格式
func convert2TsdbItem(d *cmodel.MetaData) *cmodel.TsdbItem {
	t := cmodel.TsdbItem{Tags: make(map[string]string)}

	for k, v := range d.Tags {
		t.Tags[k] = v
	}
	t.Tags["endpoint"] = d.Endpoint
	t.Metric = d.Metric
	t.Timestamp = d.Timestamp
	t.Value = d.Value
	return &t
}

func alignTs(ts int64, period int64) int64 {
	return ts - ts%period
}
