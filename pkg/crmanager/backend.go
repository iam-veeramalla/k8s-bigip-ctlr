/*-
 * Copyright (c) 2016-2019, F5 Networks, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *    http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package crmanager

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/F5Networks/k8s-bigip-ctlr/pkg/writer"

	rsc "github.com/F5Networks/k8s-bigip-ctlr/pkg/resource"
	log "github.com/F5Networks/k8s-bigip-ctlr/pkg/vlogger"
)

const (
	as3SharedApplication = "Shared"

	baseAS3Config = `{
  "$schema": "https://raw.githubusercontent.com/F5Networks/f5-appsvcs-extension/master/schema/latest/as3-schema-3.11.0-3.json",
  "class": "AS3",
  "declaration": {
    "class": "ADC",
    "schemaVersion": "3.11.0",
    "id": "urn:uuid:B97DFADF-9F0D-4F6C-8D66-E9B52E593694",
    "label": "CIS Declaration",
	"remark": "Auto-generated by CIS"
  }
}
`
)

var DEFAULT_PARTITION string

func NewAgent(params AgentParams) *Agent {
	DEFAULT_PARTITION = params.Partition + "_AS3"
	postMgr := NewPostManager(params.PostParams)
	configWriter, err := writer.NewConfigWriter()
	if nil != err {
		log.Fatalf("Failed creating ConfigWriter tool: %v", err)
	}
	agent := &Agent{
		PostManager:  postMgr,
		Partition:    params.Partition,
		ConfigWriter: configWriter,
		EventChan:    make(chan interface{}),
		activeDecl:   "",
	}
	// If running in VXLAN mode, extract the partition name from the tunnel
	// to be used in configuring a net instance of CCCL for that partition
	var vxlanPartition string
	if len(params.VXLANName) > 0 {
		cleanPath := strings.TrimLeft(params.VXLANName, "/")
		slashPos := strings.Index(cleanPath, "/")
		if slashPos == -1 {
			// No partition
			vxlanPartition = "Common"
		} else {
			// Partition and name
			vxlanPartition = cleanPath[:slashPos]
		}
	}

	gs := globalSection{
		LogLevel:       params.LogLevel,
		VerifyInterval: params.VerifyInterval,
		VXLANPartition: vxlanPartition,
	}
	bs := bigIPSection{
		BigIPUsername:   params.PostParams.BIGIPUsername,
		BigIPPassword:   params.PostParams.BIGIPPassword,
		BigIPURL:        params.PostParams.BIGIPURL,
		BigIPPartitions: []string{params.Partition},
	}

	agent.startPythonDriver(
		gs,
		bs,
		params.PythonBaseDir,
	)

	return agent
}

func (agent *Agent) Stop() {
	agent.ConfigWriter.Stop()
	agent.stopPythonDriver()
}

func (agent *Agent) PostConfig(rsCfgs ResourceConfigs) {
	decl := createAS3Declaration(rsCfgs)
	if DeepEqualJSON(agent.activeDecl, decl) {
		log.Debug("[AS3] No Change in the Configuration")
		return
	}
	agent.Write(string(decl), nil)
	agent.activeDecl = decl

	allPoolMembers := rsCfgs.GetAllPoolMembers()

	// Convert allPoolMembers to appmanger.Members so that vxlan Manger accepts
	var allPoolMems []rsc.Member

	for _, poolMem := range allPoolMembers {
		allPoolMems = append(
			allPoolMems,
			rsc.Member(poolMem),
		)
	}
	if agent.EventChan != nil {
		select {
		case agent.EventChan <- allPoolMems:
			log.Debugf("Custom Resource Manager wrote endpoints to VxlanMgr")
		case <-time.After(3 * time.Second):
		}
	}
}

//Create AS3 declaration
func createAS3Declaration(rsCfgs ResourceConfigs) as3Declaration {
	var as3Config map[string]interface{}
	_ = json.Unmarshal([]byte(baseAS3Config), &as3Config)

	adc := as3Config["declaration"].(map[string]interface{})
	for k, v := range createAS3ADC(rsCfgs) {
		adc[k] = v
	}

	decl, err := json.Marshal(as3Config)
	if err != nil {
		log.Debugf("[AS3] Unified declaration: %v\n", err)
	}
	return as3Declaration(decl)
}

func createAS3ADC(rsCfgs ResourceConfigs) as3ADC {

	// Create Shared as3Application object
	sharedApp := as3Application{}
	sharedApp["class"] = "Application"
	sharedApp["template"] = "shared"
	// Process rscfg to create AS3 Resources
	processResourcesForAS3(rsCfgs, sharedApp)
	// Create AS3 Tenant
	tenant := as3Tenant{
		"class":              "Tenant",
		as3SharedApplication: sharedApp,
	}
	as3JSONDecl := as3ADC{
		DEFAULT_PARTITION: tenant,
	}
	return as3JSONDecl
}

//Process for AS3 Resource
func processResourcesForAS3(rsCfgs ResourceConfigs, sharedApp as3Application) {
	for _, cfg := range rsCfgs {
		//Create policies
		createPoliciesDecl(cfg, sharedApp)

		//Create pools
		createPoolDecl(cfg, sharedApp)

		//Create AS3 Service for virtual server
		createServiceDecl(cfg, sharedApp)
	}
}

//Create policy declaration
func createPoliciesDecl(cfg *ResourceConfig, sharedApp as3Application) {
	_, port := extractVirtualAddressAndPort(cfg.Virtual.Destination)
	for _, pl := range cfg.Policies {
		//Create EndpointPolicy
		ep := &as3EndpointPolicy{}
		for _, rl := range pl.Rules {

			ep.Class = "Endpoint_Policy"
			s := strings.Split(pl.Strategy, "/")
			ep.Strategy = s[len(s)-1]

			//Create rules
			rulesData := &as3Rule{Name: rl.Name}

			//Create condition object
			createRuleCondition(rl, rulesData, port)

			//Creat action object
			createRuleAction(rl, rulesData)

			ep.Rules = append(ep.Rules, rulesData)
		}
		//Setting Endpoint_Policy Name
		sharedApp[pl.Name] = ep
	}
}

// Create AS3 Pools for CRD
func createPoolDecl(cfg *ResourceConfig, sharedApp as3Application) {
	for _, v := range cfg.Pools {
		pool := &as3Pool{}
		// TODO
		// pool.LoadBalancingMode = v.Balance
		pool.Class = "Pool"
		for _, val := range v.Members {
			var member as3PoolMember
			member.AddressDiscovery = "static"
			member.ServicePort = val.Port
			member.ServerAddresses = append(member.ServerAddresses, val.Address)
			pool.Members = append(pool.Members, member)
		}
		// TODO
		/**
		for _, val := range v.MonitorNames {
			var monitor as3ResourcePointer
			use := strings.Split(val, "/")
			monitor.Use = fmt.Sprintf("/%s/%s/%s",
				DEFAULT_PARTITION,
				as3SharedApplication,
				use[len(use)-1],
			)
			pool.Monitors = append(pool.Monitors, monitor)
		}
		**/
		sharedApp[v.Name] = pool
	}
}

func updateVirtualToHTTPS(v *as3Service) {
	v.Class = "Service_HTTPS"
	redirect80 := false
	v.Redirect80 = &redirect80
}

// Create AS3 Service for CRD
func createServiceDecl(cfg *ResourceConfig, sharedApp as3Application) {
	svc := &as3Service{}
	numPolicies := len(cfg.Virtual.Policies)
	switch {
	case numPolicies == 1:
		policyName := cfg.Virtual.Policies[0].Name
		svc.PolicyEndpoint = fmt.Sprintf("/%s/%s/%s",
			DEFAULT_PARTITION,
			as3SharedApplication,
			policyName)
	case numPolicies > 1:
		var peps []as3ResourcePointer
		for _, pep := range cfg.Virtual.Policies {
			svc.PolicyEndpoint = append(
				peps,
				as3ResourcePointer{
					BigIP: fmt.Sprintf("/%s/%s/%s",
						DEFAULT_PARTITION,
						as3SharedApplication,
						pep.Name,
					),
				},
			)
		}
		svc.PolicyEndpoint = peps
	case numPolicies == 0:
		// No policies since we need to handle the pool name.
		ps := strings.Split(cfg.Virtual.PoolName, "/")
		if cfg.Virtual.PoolName != "" {
			svc.Pool = fmt.Sprintf("/%s/%s/%s",
				DEFAULT_PARTITION,
				as3SharedApplication,
				ps[len(ps)-1])
		}
	}

	svc.Layer4 = cfg.Virtual.IpProtocol
	svc.Source = "0.0.0.0/0"
	svc.TranslateServerAddress = true
	svc.TranslateServerPort = true

	svc.Class = "Service_HTTP"

	virtualAddress, port := extractVirtualAddressAndPort(cfg.Virtual.Destination)
	// verify that ip address and port exists.
	if virtualAddress != "" && port != 0 {
		va := append(svc.VirtualAddresses, virtualAddress)
		svc.VirtualAddresses = va
		svc.VirtualPort = port
	}

	svc.SNAT = "auto"
	for _, v := range cfg.Virtual.IRules {
		splits := strings.Split(v, "/")
		iRuleName := splits[len(splits)-1]
		svc.IRules = append(svc.IRules, iRuleName)
	}

	sharedApp[cfg.Virtual.Name] = svc
}

// Create AS3 Rule Condition for CRD
func createRuleCondition(rl *Rule, rulesData *as3Rule, port int) {
	for _, c := range rl.Conditions {
		condition := &as3Condition{}
		if c.Host {
			condition.Name = "host"
			var values []string
			// For ports other then 80 and 443, attaching port number to host.
			// Ex. example.com:8080
			if port != 80 && port != 443 {
				for i := range c.Values {
					val := c.Values[i] + ":" + strconv.Itoa(port)
					values = append(values, val)
				}
				condition.All = &as3PolicyCompareString{
					Values: values,
				}
			} else {
				condition.All = &as3PolicyCompareString{
					Values: c.Values,
				}
			}
			if c.HTTPHost {
				condition.Type = "httpHeader"
			}
			if c.Equals {
				condition.All.Operand = "equals"
			}
		} else if c.PathSegment {
			condition.PathSegment = &as3PolicyCompareString{
				Values: c.Values,
			}
			if c.Name != "" {
				condition.Name = c.Name
			}
			condition.Index = c.Index
			if c.HTTPURI {
				condition.Type = "httpUri"
			}
			if c.Equals {
				condition.PathSegment.Operand = "equals"
			}
		} else if c.Path {
			condition.Path = &as3PolicyCompareString{
				Values: c.Values,
			}
			if c.Name != "" {
				condition.Name = c.Name
			}
			condition.Index = c.Index
			if c.HTTPURI {
				condition.Type = "httpUri"
			}
			if c.Equals {
				condition.Path.Operand = "equals"
			}
		}
		if c.Request {
			condition.Event = "request"
		}

		rulesData.Conditions = append(rulesData.Conditions, condition)
	}
}

// Create AS3 Rule Action for CRD
func createRuleAction(rl *Rule, rulesData *as3Rule) {
	for _, v := range rl.Actions {
		action := &as3Action{}
		if v.Forward {
			action.Type = "forward"
		}
		if v.Request {
			action.Event = "request"
		}
		if v.Redirect {
			action.Type = "httpRedirect"
		}
		if v.HTTPHost {
			action.Type = "httpHeader"
		}
		if v.HTTPURI {
			action.Type = "httpUri"
		}
		if v.Location != "" {
			action.Location = v.Location
		}
		// Handle hostname rewrite.
		if v.Replace && v.HTTPHost {
			action.Replace = &as3ActionReplaceMap{
				Value: v.Value,
				Name:  "host",
			}
		}
		// handle uri rewrite.
		if v.Replace && v.HTTPURI {
			action.Replace = &as3ActionReplaceMap{
				Value: v.Value,
			}
		}
		p := strings.Split(v.Pool, "/")
		if v.Pool != "" {
			action.Select = &as3ActionForwardSelect{
				Pool: &as3ResourcePointer{
					Use: p[len(p)-1],
				},
			}
		}
		rulesData.Actions = append(rulesData.Actions, action)
	}
}

//Extract virtual address and port from host URL
func extractVirtualAddressAndPort(str string) (string, int) {
	destination := strings.Split(str, "/")
	ipPort := strings.Split(destination[len(destination)-1], ":")
	// verify that ip address and port exists else log error.
	if len(ipPort) == 2 {
		port, _ := strconv.Atoi(ipPort[1])
		return ipPort[0], port
	} else {
		log.Error("Invalid Virtual Server Destination IP address/Port.")
		return "", 0
	}

}

func DeepEqualJSON(decl1, decl2 as3Declaration) bool {
	if decl1 == "" && decl2 == "" {
		return true
	}
	var o1, o2 interface{}

	err := json.Unmarshal([]byte(decl1), &o1)
	if err != nil {
		return false
	}

	err = json.Unmarshal([]byte(decl2), &o2)
	if err != nil {
		return false
	}

	return reflect.DeepEqual(o1, o2)
}
