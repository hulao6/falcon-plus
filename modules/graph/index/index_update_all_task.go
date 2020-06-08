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

package index

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	log "github.com/Sirupsen/logrus"

	nsema "github.com/toolkits/concurrent/semaphore"
	ntime "github.com/toolkits/time"

	cmodel "github.com/open-falcon/falcon-plus/common/model"
	cutils "github.com/open-falcon/falcon-plus/common/utils"
	"github.com/open-falcon/falcon-plus/modules/graph/g"
	proc "github.com/open-falcon/falcon-plus/modules/graph/proc"
)

const (
	DefaultUpdateStepInSec     = 2 * 24 * 3600 //更新步长,一定不能大于删除步长. 两天内的数据,都可以用来建立索引
	ConcurrentOfUpdateIndexAll = 1
)

var (
	semaIndexUpdateAllTask = nsema.NewSemaphore(ConcurrentOfUpdateIndexAll) //全量同步任务 并发控制器
	semaIndexUpdateAll     = nsema.NewSemaphore(4)                          // 索引全量更新时的mysql操作并发控制
)

// 索引全量更新的当前并行数
func GetConcurrentOfUpdateIndexAll() int {
	return ConcurrentOfUpdateIndexAll - semaIndexUpdateAllTask.AvailablePermits()
}

// 索引的全量更新
func UpdateIndexAllByDefaultStep() {
	UpdateIndexAll(DefaultUpdateStepInSec)
}
func UpdateIndexAll(updateStepInSec int64) {
	// 减少任务积压,但高并发时可能无效(AvailablePermits不是线程安全的)
	if semaIndexUpdateAllTask.AvailablePermits() <= 0 {
		log.Println("updateIndexAll, concurrent not available")
		return
	}

	semaIndexUpdateAllTask.Acquire()
	defer semaIndexUpdateAllTask.Release()

	startTs := time.Now().Unix()
	cnt := updateIndexAll(updateStepInSec)
	endTs := time.Now().Unix()
	log.Printf("UpdateIndexAll, lastStartTs %s, updateStepInSec %d, lastTimeConsumingInSec %d\n",
		ntime.FormatTs(startTs), updateStepInSec, endTs-startTs)

	// statistics
	proc.IndexUpdateAllCnt.SetCnt(int64(cnt))
	proc.IndexUpdateAll.Incr()
	proc.IndexUpdateAll.PutOther("lastStartTs", ntime.FormatTs(startTs))
	proc.IndexUpdateAll.PutOther("updateStepInSec", updateStepInSec)
	proc.IndexUpdateAll.PutOther("lastTimeConsumingInSec", endTs-startTs)
	proc.IndexUpdateAll.PutOther("updateCnt", cnt)
}

// 更新一条监控数据对应的索引. 用于手动添加索引,一般情况下不会使用
func UpdateIndexOne(endpoint string, metric string, tags map[string]string, dstype string, step int) error {
	itemDemo := &cmodel.GraphItem{
		Endpoint: endpoint,
		Metric:   metric,
		Tags:     tags,
		DsType:   dstype,
		Step:     step,
	}
	md5 := itemDemo.Checksum()
	uuid := itemDemo.UUID()

	cached := IndexedItemCache.Get(md5)
	if cached == nil {
		return fmt.Errorf("not found")
	}

	icitem := cached.(*IndexCacheItem)
	if icitem.UUID != uuid {
		return fmt.Errorf("bad type or step")
	}
	gitem := icitem.Item

	dbConn, err := g.GetDbConn("UpdateIndexIncrTask")
	if err != nil {
		log.Println("[ERROR] make dbConn fail", err)
		return err
	}

	return updateIndexFromOneItem(gitem, dbConn)
}

func updateIndexAll(updateStepInSec int64) int {
	var ret int = 0
	if IndexedItemCache == nil || IndexedItemCache.Size() <= 0 {
		return ret
	}

	dbConn, err := g.GetDbConn("UpdateIndexIncrTask")
	if err != nil {
		log.Println("[ERROR] make dbConn fail", err)
		return ret
	}

	// lastTs for update index
	ts := time.Now().Unix()
	lastTs := ts - updateStepInSec

	keys := IndexedItemCache.Keys()
	for _, key := range keys {
		icitem := IndexedItemCache.Get(key)
		if icitem == nil {
			continue
		}

		gitem := icitem.(*IndexCacheItem).Item
		if gitem.Timestamp < lastTs { //缓存中的数据太旧了,不能用于索引的全量更新
			IndexedItemCache.Remove(key) //在这里做个删除,有点恶心
			continue
		}
		// 并发写mysql
		semaIndexUpdateAll.Acquire()
		go func(gitem *cmodel.GraphItem, dbConn *sql.DB) {
			defer semaIndexUpdateAll.Release()
			err := updateIndexFromOneItem(gitem, dbConn)
			if err != nil {
				proc.IndexUpdateAllErrorCnt.Incr()
			}
		}(gitem, dbConn)

		ret++
	}

	return ret
}

// 根据item,更新mysql
func updateIndexFromOneItem(item *cmodel.GraphItem, conn *sql.DB) error {
	if item == nil {
		return nil
	}

	// endpoint表
	ts := item.Timestamp
	var endpointId int64 = -1

	hostExecSqlInsert := fmt.Sprintf("INSERT INTO endpoint(endpoint, ts, t_create) VALUES ('%s', '%d', NOW())", item.Endpoint, ts)

	_, errExecInsert := conn.Exec(hostExecSqlInsert)
	if errExecInsert != nil {
		log.Debugf("endpoint_insert_error:%s,%v", hostExecSqlInsert, errExecInsert)
		hostExecSqlUpdate := fmt.Sprintf("UPDATE  endpoint set ts='%d',t_modify=NOW() where endpoint='%s'", ts, item.Endpoint)
		_, errExecUpdate := conn.Exec(hostExecSqlUpdate)
		if errExecUpdate != nil {
			log.Debugf("endpoint_insertUpdate_all_failed:%s,error:%v", hostExecSqlUpdate, errExecUpdate)
		} else {
			log.Debugf("endpoint_update_success:%s", hostExecSqlUpdate)
		}
	} else {
		log.Debugf("endpoint_insert_success:%s", hostExecSqlInsert)
	}

	proc.IndexUpdateIncrDbEndpointInsertCnt.Incr()

	errHostSec := conn.QueryRow("SELECT id FROM endpoint WHERE endpoint = ?", item.Endpoint).Scan(&endpointId)
	if errHostSec != nil {
		log.Error(errHostSec)
		return errHostSec
	}
	if endpointId <= 0 {
		log.Errorf("no such endpoint in db, endpoint=%s", item.Endpoint)
		return errors.New("no such endpoint")
	}

	// tag_endpoint表
	for tagKey, tagVal := range item.Tags {
		tag := fmt.Sprintf("%s=%s", tagKey, tagVal)

		sqlTagExecInsertSql := fmt.Sprintf("INSERT INTO tag_endpoint(tag, endpoint_id, ts, t_create) VALUES ('%s', '%d', '%d', NOW())", tag, endpointId, ts)
		_, errExecTagInsert := conn.Exec(sqlTagExecInsertSql)
		if errExecTagInsert != nil {
			sqlTagExecUpdateSql := fmt.Sprintf("UPDATE tag_endpoint set ts='%d', t_modify=NOW() where endpoint_id='%d' and tag='%s' ", ts, endpointId, tag)
			_, errExecTagUpdate := conn.Exec(sqlTagExecUpdateSql)
			if errExecTagUpdate != nil {
				log.Debugf("tag_endpoint_insertUpdate_all_failed:%s,%v", sqlTagExecUpdateSql, errExecTagUpdate)
			}
		}

		proc.IndexUpdateIncrDbTagEndpointInsertCnt.Incr()
	}

	// endpoint_counter表
	counter := item.Metric
	if len(item.Tags) > 0 {
		counter = fmt.Sprintf("%s/%s", counter, cutils.SortedTags(item.Tags))
	}

	endpointCounterInsertSql := fmt.Sprintf("INSERT INTO endpoint_counter(endpoint_id,counter,step,type,ts,t_create) VALUES ('%d','%s','%d','%s','%d',NOW())",
		endpointId, counter, item.Step, item.DsType, ts)
	_, errEndContInsertErr := conn.Exec(endpointCounterInsertSql)
	if errEndContInsertErr != nil {
		endpointCounterUpdateSql := fmt.Sprintf("UPDATE endpoint_counter set ts='%d',step='%d',type='%s',t_modify=NOW() where endpoint_id='%d' and counter='%s' ", ts, item.Step, item.DsType, endpointId, counter)
		_, errEndContUpdateErr := conn.Exec(endpointCounterUpdateSql)
		if errEndContUpdateErr != nil {
			log.Debugf("endpoint_counter_insertUpdate_all_failed:%s,%v", endpointCounterUpdateSql, errEndContUpdateErr)
		} else {
			log.Debugf("endpointcounter_update_success:%s", endpointCounterUpdateSql)
		}
	} else {
		log.Debugf("endpointcounter_insert_success:%s", endpointCounterInsertSql)
	}

	proc.IndexUpdateIncrDbEndpointCounterInsertCnt.Incr()

	return nil
}
