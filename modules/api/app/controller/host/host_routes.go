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

package host

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/open-falcon/falcon-plus/modules/api/app/utils"
	"github.com/open-falcon/falcon-plus/modules/api/config"
)

var db config.DBPool

const badstatus = http.StatusBadRequest
const expecstatus = http.StatusExpectationFailed

func Routes(r *gin.Engine) {
	db = config.Con()
	grafanar := r.Group("/grafana/v1")
	hostr := r.Group("/api/v1")
	hostr.Use(utils.AuthSessionMidd)
	//hostgroup
	hostr.GET("/hostgroup", GetHostGroups)
	hostr.POST("/hostgroup", CrateHostGroup)
	hostr.POST("/hostgroup/host", BindHostToHostGroup)
	hostr.PUT("/hostgroup/host", UnBindAHostToHostGroup)
	hostr.GET("/hostgroup/:host_group", GetHostGroup)
	hostr.GET("/hostgroupsimple/:host_group", GetHostGroupSimple)
	hostr.GET("/hostgrouphosts/:host_group", GetHostGroupHosts)
	hostr.GET("/hostgroupstag", GetGraphEndpointByGroupName)
	hostr.PUT("/hostgroup", PutHostGroup)
	hostr.DELETE("/hostgroup/:host_group", DeleteHostGroup)
	hostr.PATCH("/hostgroup/:host_group/host", PatchHostGroupHost)

	//plugins
	hostr.GET("/hostgroup/:host_group/plugins", GetPluginOfGrp)
	hostr.POST("/plugin", CreatePlugin)
	hostr.DELETE("/plugin/:id", DeletePlugin)

	//aggreator
	hostr.GET("/hostgroup/:host_group/aggregators", GetAggregatorListOfGrp)
	hostr.GET("/aggregator/:id", GetAggregator)
	hostr.POST("/aggregator", CreateAggregator)
	hostr.PUT("/aggregator", UpdateAggregator)
	hostr.DELETE("/aggregator/:id", DeleteAggregator)

	//template
	hostr.POST("/hostgroup/template", BindTemplateToGroup)
	hostr.PUT("/hostgroup/template", UnBindTemplateToGroup)
	hostr.GET("/hostgroup/:host_group/template", GetTemplateOfHostGroup)

	//host
	hostr.GET("/host/:host_id/template", GetTplsRelatedHost)
	hostr.GET("/host/:host_id/hostgroup", GetGrpsRelatedHost)

	//maintain
	hostr.POST("/host/maintain", SetMaintain)
	hostr.DELETE("/host/maintain", UnsetMaintain)

	//jump to grafana
	grafanar.GET("/jump_to_grafana/:endpoint", GrafanaUrlHandler)
}
