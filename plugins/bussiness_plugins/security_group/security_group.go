package securitygroup

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/WeBankPartners/wecube-plugins-qcloud/plugins"
	"github.com/sirupsen/logrus"
	vpc "github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/vpc/v20170312"
)

const (
	MAX_SEUCRITY_RULE_NUM = 100
)

//interface definition
type ResourceInstance interface {
	ResourceTypeName() string
	GetId() string
	GetName() string
	GetRegion() string
	GetIp() string
	QuerySecurityGroups(providerParams string) ([]string, error)
	AssociateSecurityGroups(providerParams string, securityGroups []string) error
	IsSupportSecurityGroupApi() bool
	GetBackendTargets(providerParams string, proto string, port string) ([]ResourceInstance, []string, error)
}

type ResourceType interface {
	QueryInstancesById(providerParams string, instanceIds []string) (map[string]ResourceInstance, error)
	QueryInstancesByIp(providerParams string, ips []string) (map[string]ResourceInstance, error)
	IsLoadBalanceType() bool
	IsSupportEgressPolicy() bool
}

//resourceType register
var (
	resTypesMutex   sync.Mutex
	resourceTypeMap = make(map[string]ResourceType)
)

//resource type  register
func addNewResourceType(name string, newResourceType ResourceType) error {
	resTypesMutex.Lock()
	defer resTypesMutex.Unlock()

	if _, found := resourceTypeMap[name]; found {
		logrus.Errorf("resourceType(%s) was registered twice", name)
	}

	resourceTypeMap[name] = newResourceType
	return nil
}

func getResouceTypeByName(name string) (ResourceType, error) {
	resTypesMutex.Lock()
	defer resTypesMutex.Unlock()

	resType, found := resourceTypeMap[name]
	if !found {
		return nil, fmt.Errorf("resourceType[%s] not found", name)
	}
	return resType, nil
}

type BussinessSecurityGroupPlugin struct {
}

func (plugin *BussinessSecurityGroupPlugin) GetActionByName(actionName string) (plugins.Action, error) {
	action, found := SecurityGroupActions[actionName]

	if !found {
		return nil, fmt.Errorf("Bussiness Security Group plugin,action = %s not found", actionName)
	}

	return action, nil
}

var SecurityGroupActions = make(map[string]plugins.Action)

func init() {
	//plugin registry
	plugins.RegisterPlugin("bs-security-group", new(BussinessSecurityGroupPlugin))

	//resourceType registry
	addNewResourceType("mysql", new(MysqlResourceType))
	addNewResourceType("cvm", new(CvmResourceType))
	addNewResourceType("clb", new(ClbResourceType))
	addNewResourceType("redis", new(RedisResourceType))
	addNewResourceType("mongodb", new(MongodbResourceType))

	//action
	SecurityGroupActions["calc-security-policies"] = new(CalcSecurityPolicyAction)
	SecurityGroupActions["apply-security-policies"] = new(ApplySecurityPolicyAction)
}

func findInstanceByIp(ip string) (ResourceInstance, error) {
	regions, err := getRegions()
	if err != nil {
		logrus.Errorf("getRegions meet err=%v\n", err)
		return nil, err
	}

	for _, region := range regions {
		providerParams, err := getProviderParams(region)
		if err != nil {
			logrus.Errorf("getProviderParams meet err=%v\n", err)
			return nil, err
		}

		for _, resType := range resourceTypeMap {
			instanceMap, err := resType.QueryInstancesByIp(providerParams, []string{ip})
			logrus.Infof("instanceMap:%++v", instanceMap)
			if err != nil {
				logrus.Errorf("QueryInstancesByIp meet err=%v\n", err)
				return nil, err
			}
			instance, ok := instanceMap[ip]
			if ok {
				return instance, nil
			}
		}
	}

	logrus.Errorf("ip(%s),can't be found", ip)
	return nil, fmt.Errorf("ip(%s),can't be found", ip)
}

//---------------calc security policy action------------------------------//
type CalcSecurityPoliciesRequest struct {
	Protocol         string   `json:"protocol"`
	SourceIps        []string `json:"source_ips"`
	DestIps          []string `json:"dest_ips"`
	DestPort         string   `json:"dest_port"`
	PolicyAction     string   `json:"policy_action"`
	PolicyDirections []string `json:"policy_directions"`
	Description      string   `json:"description"`
}

type SecurityPolicy struct {
	Ip                      string `json:"ip"`
	Type                    string `json:"type"`
	Id                      string `json:"id"`
	Region                  string `json:"region"`
	SupportSecurityGroupApi bool   `json:"support_security_group_api"`
	PeerIp                  string `json:"peer_ip"`
	Protocol                string `json:"protocol"`
	Ports                   string `json:"ports"`
	Action                  string `json:"action"`
	Description             string `json:"description"`
	ErrorMsg                string `json:"err_msg,omitempty"`
	UndoReason              string `json:"undo_reason,omitempty"`
	SecurityGroupId         string `json:"security_group_id,omitempty"`
}

type CalcSecurityPoliciesResult struct {
	TimeTaken string `json:"time_taken"`

	IngressPoliciesTotal int `json:"ingress_policies_total"`
	EgressPoliciesTotal  int `json:"egress_policies_total"`

	IngressPolicies []SecurityPolicy `json:"ingress_policies"`
	EgressPolicies  []SecurityPolicy `json:"egress_policies"`
}

type CalcSecurityPolicyAction struct {
}

func (action *CalcSecurityPolicyAction) ReadParam(param interface{}) (interface{}, error) {
	var input CalcSecurityPoliciesRequest
	err := plugins.UnmarshalJson(param, &input)
	if err != nil {
		logrus.Errorf("CalcSecurityPolicyAction unmarshal failed,err=%v,param=%v", err, param)
		return nil, err
	}
	return input, nil
}

func (action *CalcSecurityPolicyAction) CheckParam(input interface{}) error {
	req, _ := input.(CalcSecurityPoliciesRequest)
	if err := isValidProtocol(req.Protocol); err != nil {
		return err
	}

	if err := isValidAction(req.PolicyAction); err != nil {
		return err
	}

	for _, ip := range req.SourceIps {
		if err := isValidIp(ip); err != nil {
			return err
		}
	}

	for _, ip := range req.DestIps {
		if err := isValidIp(ip); err != nil {
			return err
		}
	}

	_, err := getPortsByPolicyFormat(req.DestPort)
	if err != nil {
		return err
	}

	for _, direction := range req.PolicyDirections {
		if err := isValidDirection(direction); err != nil {
			return err
		}
	}

	return nil
}

func newPolicies(instance ResourceInstance, myIp string, peerIp string, proto string, port string, action string, desc string) ([]SecurityPolicy, error) {
	policies := []SecurityPolicy{}
	resType, _ := getResouceTypeByName(instance.ResourceTypeName())

	//非LB设备
	if false == resType.IsLoadBalanceType() {
		newPolicy := SecurityPolicy{
			Ip:                      myIp,
			Type:                    instance.ResourceTypeName(),
			Id:                      instance.GetId(),
			Region:                  instance.GetRegion(),
			SupportSecurityGroupApi: instance.IsSupportSecurityGroupApi(),
			PeerIp:                  peerIp,
			Protocol:                proto,
			Ports:                   port,
			Action:                  action,
			Description:             desc,
		}
		policies := append(policies, newPolicy)
		return policies, nil
	}

	//LB设备
	providerParams, _ := getProviderParams(instance.GetRegion())
	splitPorts := strings.Split(port, ",")

	for _, splitPort := range splitPorts {
		if _, err := strconv.Atoi(splitPort); err != nil {
			return policies, fmt.Errorf("loadbalancer do not support port format like %s", port)
		}
		instances, ports, err := instance.GetBackendTargets(providerParams, proto, splitPort)
		fmt.Printf("getLb backendHost=%++v\n", instances)
		if err != nil {
			return policies, err
		}
		if len(instances) == 0 {
			return policies, fmt.Errorf("loadbalancer(%s) port (%v) do not have any backends", instance.GetIp(), splitPort)
		}

		for i, backendInstance := range instances {
			newPolicy := SecurityPolicy{
				Ip:                      backendInstance.GetIp(),
				Type:                    backendInstance.ResourceTypeName(),
				Id:                      backendInstance.GetId(),
				Region:                  backendInstance.GetRegion(),
				SupportSecurityGroupApi: backendInstance.IsSupportSecurityGroupApi(),
				PeerIp:                  peerIp,
				Protocol:                proto,
				Ports:                   ports[i],
				Action:                  action,
				Description:             desc,
			}
			policies = append(policies, newPolicy)
		}
	}

	return policies, nil
}

func calcPolicies(devIp string, peerIps []string, proto string, ports []string,
	action string, description string, direction string) ([]SecurityPolicy, error) {
	policies := []SecurityPolicy{}

	//check if dev exist
	instance, err := findInstanceByIp(devIp)
	if err != nil {
		return policies, err
	}

	resType, err := getResouceTypeByName(instance.ResourceTypeName())
	if err != nil {
		return policies, err
	}

	if direction == EGRESS_RULE {
		if false == resType.IsSupportEgressPolicy() {
			logrus.Errorf("%s is %s device,do not support egress", devIp, instance.ResourceTypeName())
			return policies, nil
		}
	}

	for _, peerIp := range peerIps {
		peerInstance, err := findInstanceByIp(peerIp)
		fmt.Printf("findInstanceByip peerIp=%s,instance=%++v,err=%v\n", peerIp, peerInstance, err)
		if err == nil {
			peerResType, _ := getResouceTypeByName(peerInstance.ResourceTypeName())
			if direction == INGRESS_RULE && nil != peerResType && peerResType.IsLoadBalanceType() {
				return policies, fmt.Errorf("对端设备(%s) 是负载均衡设备,入栈规则不支持对端IP为负载均衡设备", peerIp)
			}
		}

		for _, port := range ports {
			newPolicies, err := newPolicies(instance, devIp, peerIp, proto, port, action, description)
			if err != nil {
				return policies, err
			}
			if len(newPolicies) > 0 {
				policies = append(policies, newPolicies...)
			}
		}
	}
	return policies, nil
}

func (action *CalcSecurityPolicyAction) Do(input interface{}) (interface{}, error) {
	var finalError error
	req, _ := input.(CalcSecurityPoliciesRequest)
	result := CalcSecurityPoliciesResult{}
	start := time.Now()
	ports, _ := getPortsByPolicyFormat(req.DestPort)

	//calc egress policies
	if isContainInList(EGRESS_RULE, req.PolicyDirections) {
		for _, ip := range req.SourceIps {
			policies, err := calcPolicies(ip, req.DestIps, req.Protocol, ports, req.PolicyAction, req.Description, EGRESS_RULE)
			result.EgressPolicies = append(result.EgressPolicies, policies...)
			if err != nil && finalError != nil {
				finalError = fmt.Errorf("%s", finalError.Error()+err.Error())
			}
			if err != nil && finalError == nil {
				finalError = err
			}
		}
	}

	//calc ingress policies
	if isContainInList(INGRESS_RULE, req.PolicyDirections) {
		for _, ip := range req.DestIps {
			policies, err := calcPolicies(ip, req.SourceIps, req.Protocol, ports, req.PolicyAction, req.Description, INGRESS_RULE)
			result.IngressPolicies = append(result.IngressPolicies, policies...)
			if err != nil && finalError != nil {
				finalError = fmt.Errorf("%s", finalError.Error()+err.Error())
			}
			if err != nil && finalError == nil {
				finalError = err
			}
		}
	}

	result.TimeTaken = fmt.Sprintf("%v", time.Since(start))
	result.IngressPoliciesTotal = len(result.IngressPolicies)
	result.EgressPoliciesTotal = len(result.EgressPolicies)

	return result, finalError
}

//---------------apply security policy action------------------------------//
type ApplySecurityPolicyAction struct {
}

type ApplySecurityPoliciesRequest struct {
	IngressPolicies []SecurityPolicy `json:"ingress_policies"`
	EgressPolicies  []SecurityPolicy `json:"egress_policies"`
}

type ApplyResult struct {
	PoliciesTotal int `json:"policies_total"`

	SuccessTotal int `json:"success_policies_total"`
	UndoTotal    int `json:"undo_policies_total"`
	FailedTotal  int `json:"failed_policies_total"`

	SuccessPolicies []SecurityPolicy `json:"success_policies"`
	UndoPolicies    []SecurityPolicy `json:"undo_policies"`
	FailedPolicies  []SecurityPolicy `json:"failed_policies"`
}

type ApplySecurityPoliciesResult struct {
	TimeTaken          string      `json:"time_taken"`
	IngressApplyResult ApplyResult `json:"ingress"`
	EgressApplyResult  ApplyResult `json:"egress"`
}

func (action *ApplySecurityPolicyAction) ReadParam(param interface{}) (interface{}, error) {
	var input ApplySecurityPoliciesRequest
	err := plugins.UnmarshalJson(param, &input)
	if err != nil {
		logrus.Errorf("ApplySecurityPolicyAction:unmarshal failed,err=%v,param=%v", err, param)
		return nil, err
	}
	return input, nil
}

func (action *ApplySecurityPolicyAction) CheckParam(input interface{}) error {
	req, _ := input.(ApplySecurityPoliciesRequest)

	for _, policy := range req.IngressPolicies {
		if policy.Ip == "" || policy.Id == "" {
			return errors.New("ingress policy have empty value")
		}
	}

	for _, policy := range req.EgressPolicies {
		if policy.Ip == "" || policy.Id == "" {
			return errors.New("egress policy have empty value")
		}
	}

	return nil
}

func (action *ApplySecurityPolicyAction) Do(input interface{}) (interface{}, error) {
	var err error
	req, _ := input.(ApplySecurityPoliciesRequest)
	result := ApplySecurityPoliciesResult{}
	start := time.Now()

	result.IngressApplyResult = applyPolicies(req.IngressPolicies, INGRESS_RULE)
	result.EgressApplyResult = applyPolicies(req.EgressPolicies, EGRESS_RULE)

	result.TimeTaken = fmt.Sprintf("%v", time.Since(start))
	if result.IngressApplyResult.FailedTotal > 0 || result.EgressApplyResult.FailedTotal > 0 {
		err = errors.New("have some failed polices,please check policy applied detail")
	}

	return result, err
}

func fillSecuityPoliciesWithErrMsg(policies []*SecurityPolicy, err error) {
	for _, policy := range policies {
		policy.ErrorMsg = err.Error()
	}
}

func applyPolicies(policies []SecurityPolicy, direction string) ApplyResult {
	result := ApplyResult{}
	instanceMap := make(map[string][]*SecurityPolicy)

	for i, _ := range policies {
		if strings.HasPrefix(policies[i].Type, "clb-cvm") {
			policies[i].Type = "cvm"
		}

		if policies[i].SupportSecurityGroupApi == true {
			key := policies[i].Ip
			instanceMap[key] = append(instanceMap[key], &policies[i])
		} else {
			policies[i].UndoReason = fmt.Sprintf("instanceType(%s) do not support security group api", policies[i].Type)
			result.UndoPolicies = append(result.UndoPolicies, policies[i])
		}
	}

	for _, policies := range instanceMap {
		resType, err := getResouceTypeByName(policies[0].Type)
		if err != nil {
			fillSecuityPoliciesWithErrMsg(policies, err)
			continue
		}

		providerParams, err := getProviderParams(policies[0].Region)
		if err != nil {
			fillSecuityPoliciesWithErrMsg(policies, err)
			continue
		}

		instances, err := resType.QueryInstancesById(providerParams, []string{policies[0].Id})
		if err != nil {
			fillSecuityPoliciesWithErrMsg(policies, err)
			continue
		}
		if len(instances) == 0 {
			fillSecuityPoliciesWithErrMsg(policies, fmt.Errorf("can't found instanceId(%s)", policies[0].Id))
			continue
		}
		instance := instances[policies[0].Id]
		logrus.Infof("applyPolicies instance=%++v", instance)

		existSecurityGroups, err := instance.QuerySecurityGroups(providerParams)
		if err != nil {
			logrus.Infof("applyPolicies err=%v", err)
			fillSecuityPoliciesWithErrMsg(policies, err)
			continue
		}

		logrus.Infof("applyPolicies existSecurityGroups=%++v", existSecurityGroups)
		newSecurityGroups, err := createPolicies(providerParams, existSecurityGroups, policies, direction)
		if err != nil {
			destroyPolicies(providerParams, policies, direction)
			fillSecuityPoliciesWithErrMsg(policies, err)
			continue
		}
		logrus.Infof("newSecurityGroups:%v", newSecurityGroups)

		if len(newSecurityGroups) > 0 {
			groups := []string{}
			groups = append(groups, newSecurityGroups...)
			groups = append(groups, existSecurityGroups...)

			if err = instance.AssociateSecurityGroups(providerParams, groups); err != nil {
				destroyPolicies(providerParams, policies, direction)
				bindError := fmt.Errorf("resourceType(%s) instance(%s) AssociateSecurityGroups[%v] meet err=%v", policies[0].Type, policies[0].Ip, groups, err)
				fillSecuityPoliciesWithErrMsg(policies, bindError)
				continue
			}
		}
	}

	for _, policies := range instanceMap {
		for _, policy := range policies {
			if policy.ErrorMsg == "" {
				result.SuccessPolicies = append(result.SuccessPolicies, *policy)
			} else {
				result.FailedPolicies = append(result.FailedPolicies, *policy)
			}
		}
	}
	result.PoliciesTotal = len(policies)
	result.SuccessTotal = len(result.SuccessPolicies)
	result.FailedTotal = len(result.FailedPolicies)

	return result
}

//自动构建的安全组的名称格式ip-auoto-1,ip_auto_2
func getAutoCreatedSecurityGroups(ip string, allSecurityGroupsNames, allSecurityGroupsIds []string) ([]string, int, error) {
	var err error
	maxAutoCreatedNum := 0
	createdSecurityGroups := []string{}
	nums := []int{}
	for i, securityGroup := range allSecurityGroupsNames {
		elements := strings.Split(securityGroup, "-")
		if len(elements) == 3 {
			if elements[0] == ip && elements[1] == "auto" {
				if num, err := strconv.Atoi(elements[2]); err == nil {
					createdSecurityGroups = append(createdSecurityGroups, allSecurityGroupsIds[i])
					nums = append(nums, num)
					if maxAutoCreatedNum < num {
						maxAutoCreatedNum = num
					}
				}
			}
		}
	}
	createdSecurityGroups, err = sortSecurityGroupsIds(nums, createdSecurityGroups)
	if err != nil {
		return createdSecurityGroups, maxAutoCreatedNum + 1, err
	}
	logrus.Infof("getAutoCreatedSecurityGroups createdSecurityGroups:%v", createdSecurityGroups)

	return createdSecurityGroups, maxAutoCreatedNum + 1, nil
}

func sortSecurityGroupsIds(num []int, securityGroupsIds []string) ([]string, error) {
	if len(num) != len(securityGroupsIds) {
		err := fmt.Errorf("sortSecurityGroupsIds error: lengths of two arrays is not equal")
		return []string{}, err
	}
	flag := 1
	for i := 0; i < len(num) && flag == 1; i++ {
		flag = 0
		for j := 0; j < len(num)-i-1; j++ {
			if num[j] > num[j+1] {
				num[j], num[j+1] = num[j+1], num[j]
				securityGroupsIds[j], securityGroupsIds[j+1] = securityGroupsIds[j+1], securityGroupsIds[j]
				flag = 1
			}
		}
	}
	return securityGroupsIds, nil
}

func getSecurityGroupFreePolicyNum(providerParams string, securityGroup string, direction string) (int, error) {
	policiesSet, err := plugins.QuerySecurityGroupPolicies(providerParams, securityGroup)
	if err != nil {
		logrus.Errorf("getSecurityGroupFreePolicyNum meet err=%v\n", err)
		return 0, err
	}

	if strings.EqualFold(direction, INGRESS_RULE) {
		return MAX_SEUCRITY_RULE_NUM - len(policiesSet.Ingress), nil
	}

	return MAX_SEUCRITY_RULE_NUM - len(policiesSet.Egress), nil
}

func getSecurityGroupNames(providerParams string, securityGroupIds []string) ([]string, error) {
	securityGroupNames := []string{}
	idNameMap := make(map[string]string)
	securityGroupSet, err := plugins.QuerySecurityGroups(providerParams, securityGroupIds)
	if err != nil {
		return securityGroupNames, err
	}

	for _, securityGroup := range securityGroupSet {
		idNameMap[*securityGroup.SecurityGroupId] = *securityGroup.SecurityGroupName
	}

	for _, id := range securityGroupIds {
		if name, ok := idNameMap[id]; ok {
			securityGroupNames = append(securityGroupNames, name)
		} else {
			return securityGroupNames, fmt.Errorf("can't found groupId(%s) detail", id)
		}
	}

	return securityGroupNames, nil
}

//format ip-auto-2
func createNewAutomationSecurityGroups(providerParams string, ip string, newCreatedSecurityGroupNum int, auotNumIndex int) ([]string, error) {
	newSecurityGroupIds := []string{}
	for i := 0; i < newCreatedSecurityGroupNum; i++ {
		securityGroupName := fmt.Sprintf("%s-auto-%d", ip, auotNumIndex+i)
		securityGroupId, err := plugins.CreateSecurityGroup(providerParams, securityGroupName, "automation created")
		if err != nil {
			logrus.Errorf("CreateSecurityGroup meet err=%v", err)
			return newSecurityGroupIds, err
		}
		newSecurityGroupIds = append(newSecurityGroupIds, securityGroupId)
	}

	return newSecurityGroupIds, nil
}

func newSecurityPolicySet(policies []*SecurityPolicy, direction string, isSetPolicyIndex bool) vpc.SecurityGroupPolicySet {
	securityPolicies := []*vpc.SecurityGroupPolicy{}
	var policyIndex int64 = 0

	for _, policy := range policies {
		action := strings.ToUpper(policy.Action)
		securityPolicy := vpc.SecurityGroupPolicy{
			Protocol:          &policy.Protocol,
			Port:              &policy.Ports,
			CidrBlock:         &policy.PeerIp,
			Action:            &action,
			PolicyDescription: &policy.Description,
		}
		if isSetPolicyIndex {
			securityPolicy.PolicyIndex = &policyIndex
		}
		securityPolicies = append(securityPolicies, &securityPolicy)
	}

	securityGroupPolicySet := vpc.SecurityGroupPolicySet{}
	if strings.EqualFold(direction, INGRESS_RULE) {
		securityGroupPolicySet.Ingress = securityPolicies
	} else {
		securityGroupPolicySet.Egress = securityPolicies
	}

	return securityGroupPolicySet
}

func addPoliciesToSecurityGroup(providerParams string, securityGroupId string, policies []*SecurityPolicy, direction string) error {
	req := vpc.NewCreateSecurityGroupPoliciesRequest()
	req.SecurityGroupId = &securityGroupId
	var err error

	if len(policies) == 0 {
		return nil
	}
	defer func() {
		if err != nil {
			logrus.Errorf("add policy to securityGroup(%s) meet err =%v", securityGroupId, err)
			errMsg := fmt.Sprintf("add policy to securityGroup(%s) meet err =%v", securityGroupId, err)
			for _, policy := range policies {
				policy.ErrorMsg = errMsg
			}
		}
	}()

	paramsMap, err := plugins.GetMapFromProviderParams(providerParams)
	client, err := plugins.CreateVpcClient(paramsMap["Region"], paramsMap["SecretID"], paramsMap["SecretKey"])
	if err != nil {
		return err
	}

	securityGroupPolicySet := newSecurityPolicySet(policies, direction, true)
	req.SecurityGroupPolicySet = &securityGroupPolicySet
	if _, err = client.CreateSecurityGroupPolicies(req); err == nil {
		for _, policy := range policies {
			policy.SecurityGroupId = securityGroupId
		}
	}

	return err
}

func createPolicies(providerParams string, existSecurityGroups []string, policies []*SecurityPolicy, direction string) ([]string, error) {
	newSecurityGroups := []string{}
	freePolicyNumMap := make(map[string]int)
	freePoliciesNum := 0
	securityGroupsIds := []string{}

	if len(policies) == 0 {
		return newSecurityGroups, nil
	}

	securityGroupsNames, err := getSecurityGroupNames(providerParams, existSecurityGroups)
	if err != nil {
		return newSecurityGroups, err
	}
	logrus.Infof("securityGroupsNames:%v", securityGroupsNames)

	createdSecurityGroups, autoCreatedStartIndex, err := getAutoCreatedSecurityGroups(policies[0].Ip, securityGroupsNames, existSecurityGroups)
	if err != nil {
		return newSecurityGroups, err
	}
	logrus.Infof("createdSecurityGroups=%v, autoCreatedStartIndex=%v", createdSecurityGroups, autoCreatedStartIndex)

	//计算已经存在的安全组中还能插入多少条
	for _, securityGroup := range createdSecurityGroups {
		freeNum, err := getSecurityGroupFreePolicyNum(providerParams, securityGroup, direction)
		if err != nil {
			return newSecurityGroups, err
		}
		freePolicyNumMap[securityGroup] = freeNum
		freePoliciesNum += freeNum
	}
	securityGroupsIds = append(securityGroupsIds, createdSecurityGroups...)

	//计算需要新创建几个安全组
	if freePoliciesNum < len(policies) {
		newSecurityGroupNum := (len(policies) - freePoliciesNum + MAX_SEUCRITY_RULE_NUM - 1) / MAX_SEUCRITY_RULE_NUM
		newSecurityGroups, err = createNewAutomationSecurityGroups(providerParams, policies[0].Ip, newSecurityGroupNum, autoCreatedStartIndex)
		if err != nil {
			return newSecurityGroups, err
		}
		logrus.Infof("newSecurityGroups=%v", newSecurityGroups)
		securityGroupsIds = append(securityGroupsIds, newSecurityGroups...)

		for _, securityGroup := range newSecurityGroups {
			freePolicyNumMap[securityGroup] = MAX_SEUCRITY_RULE_NUM
		}
	}

	logrus.Infof("freePolicyNumMap=%v", freePolicyNumMap)
	//开始将策略加到安全组中
	offset, limit := 0, 0

	//for securityGroup, freeNum := range freePolicyNumMap {
	for _, securityGroupId := range securityGroupsIds {
		freeNum := freePolicyNumMap[securityGroupId]
		if len(policies)-offset > freeNum {
			limit = freeNum
		} else {
			limit = len(policies) - offset
		}
		if err := addPoliciesToSecurityGroup(providerParams, securityGroupId, policies[offset:offset+limit], direction); err != nil {
			return newSecurityGroups, err
		}

		for i := offset; i < offset+limit; i++ {
			policies[i].SecurityGroupId = securityGroupId
		}
		offset += limit
	}
	return newSecurityGroups, nil
}

func destroyPolicies(providerParams string, policies []*SecurityPolicy, direction string) error {
	securityGroupMap := make(map[string][]*SecurityPolicy)
	for _, policy := range policies {
		securityGroupMap[policy.SecurityGroupId] = append(securityGroupMap[policy.SecurityGroupId], policy)
		logrus.Infof("destroyPolicies policy=%++v", *policy)
	}

	paramsMap, err := plugins.GetMapFromProviderParams(providerParams)
	client, err := plugins.CreateVpcClient(paramsMap["Region"], paramsMap["SecretID"], paramsMap["SecretKey"])
	if err != nil {
		return err
	}

	for securityGroupId, policies := range securityGroupMap {
		securityGroupPolicySet := newSecurityPolicySet(policies, direction, false)
		req := vpc.NewDeleteSecurityGroupPoliciesRequest()
		req.SecurityGroupId = &securityGroupId
		req.SecurityGroupPolicySet = &securityGroupPolicySet

		_, err := client.DeleteSecurityGroupPolicies(req)
		if err != nil {
			logrus.Errorf("DeleteSecurityGroupPolicies meet err=%v,req=%++v", err, *req)
			return err
		}
	}
	return nil
}
