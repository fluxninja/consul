// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: BUSL-1.1

package xdsv2

import (
	"fmt"
	envoy_core_v3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	envoy_matcher_v3 "github.com/envoyproxy/go-control-plane/envoy/type/matcher/v3"
	"github.com/hashicorp/consul/agent/xds/response"
	"github.com/hashicorp/consul/envoyextensions/xdscommon"
	"github.com/hashicorp/consul/proto-public/pbmesh/v1alpha1/pbproxystate"
	"google.golang.org/protobuf/types/known/wrapperspb"
	"strings"

	envoy_route_v3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	"google.golang.org/protobuf/proto"
)

func (pr *ProxyResources) makeXDSRoutes() ([]proto.Message, error) {
	routes := make([]proto.Message, 0)

	for name, r := range pr.proxyState.Routes {
		protoRoute := pr.makeEnvoyRouteConfigFromProxystateRoute(name, r)
		// TODO: aggregate errors for routes and still return any properly formed routes.
		routes = append(routes, protoRoute)
	}

	return routes, nil
}

func (pr *ProxyResources) makeEnvoyRoute(name string) (*envoy_route_v3.RouteConfiguration, error) {
	var route *envoy_route_v3.RouteConfiguration
	// TODO(proxystate): This will make routes in the future. This function should distinguish between static routes
	// inlined into listeners and non-static routes that should be added as top level Envoy resources.
	_, ok := pr.proxyState.Routes[name]
	if !ok {
		// This should not happen with a valid proxy state.
		return nil, fmt.Errorf("could not find route in ProxyState: %s", name)

	}
	return route, nil
}

// makeEnvoyRouteConfigFromProxystateRoute converts the proxystate representation of a Route into Envoy proto message
// form. We don't throw any errors here, since the proxystate has already been validated.
func (pr *ProxyResources) makeEnvoyRouteConfigFromProxystateRoute(name string, psRoute *pbproxystate.Route) *envoy_route_v3.RouteConfiguration {

	envoyRouteConfig := &envoy_route_v3.RouteConfiguration{
		Name: name,
		// ValidateClusters defaults to true when defined statically and false
		// when done via RDS. Re-set the reasonable value of true to prevent
		// null-routing traffic.
		ValidateClusters: response.MakeBoolValue(true),
	}

	for _, vh := range psRoute.GetVirtualHosts() {
		envoyRouteConfig.VirtualHosts = append(envoyRouteConfig.VirtualHosts, pr.makeEnvoyVHFromProxystateVH(vh))
	}

	return envoyRouteConfig
}

func (pr *ProxyResources) makeEnvoyVHFromProxystateVH(psVirtualHost *pbproxystate.VirtualHost) *envoy_route_v3.VirtualHost {
	envoyVirtualHost := &envoy_route_v3.VirtualHost{
		Name:    psVirtualHost.Name,
		Domains: psVirtualHost.GetDomains(),
	}

	for _, rr := range psVirtualHost.GetRouteRules() {
		envoyVirtualHost.Routes = append(envoyVirtualHost.Routes, pr.makeEnvoyRouteFromProxystateRouteRule(rr))
	}

	for _, hm := range psVirtualHost.GetHeaderMutations() {
		injectEnvoyVirtualHostWithProxystateHeaderMutation(envoyVirtualHost, hm)
	}

	return envoyVirtualHost
}

func (pr *ProxyResources) makeEnvoyRouteFromProxystateRouteRule(psRouteRule *pbproxystate.RouteRule) *envoy_route_v3.Route {
	envoyRouteRule := &envoy_route_v3.Route{
		Match:  makeEnvoyRouteMatchFromProxystateRouteMatch(psRouteRule.GetMatch()),
		Action: pr.makeEnvoyRouteActionFromProxystateRouteDestination(psRouteRule.GetDestination()),
	}

	for _, hm := range psRouteRule.GetHeaderMutations() {
		injectEnvoyRouteRuleWithProxystateHeaderMutation(envoyRouteRule, hm)
	}

	return envoyRouteRule
}

func makeEnvoyRouteMatchFromProxystateRouteMatch(psRouteMatch *pbproxystate.RouteMatch) *envoy_route_v3.RouteMatch {
	envoyRouteMatch := &envoy_route_v3.RouteMatch{}

	switch psRouteMatch.PathMatch.GetPathMatch().(type) {
	case *pbproxystate.PathMatch_Exact:
		envoyRouteMatch.PathSpecifier = &envoy_route_v3.RouteMatch_Path{
			Path: psRouteMatch.PathMatch.GetExact(),
		}
	case *pbproxystate.PathMatch_Prefix:
		envoyRouteMatch.PathSpecifier = &envoy_route_v3.RouteMatch_Prefix{
			Prefix: psRouteMatch.PathMatch.GetPrefix(),
		}
	case *pbproxystate.PathMatch_Regex:
		envoyRouteMatch.PathSpecifier = &envoy_route_v3.RouteMatch_SafeRegex{
			SafeRegex: makeEnvoyRegexMatch(psRouteMatch.PathMatch.GetRegex()),
		}
	default:
		// This shouldn't be possible considering the types of PathMatch
		return nil
	}

	if len(psRouteMatch.GetHeaderMatches()) > 0 {
		envoyRouteMatch.Headers = make([]*envoy_route_v3.HeaderMatcher, 0, len(psRouteMatch.GetHeaderMatches()))
	}
	for _, psHM := range psRouteMatch.GetHeaderMatches() {
		envoyRouteMatch.Headers = append(envoyRouteMatch.Headers, makeEnvoyHeaderMatcherFromProxystateHeaderMatch(psHM))
	}

	if len(psRouteMatch.MethodMatches) > 0 {
		methodHeaderRegex := strings.Join(psRouteMatch.MethodMatches, "|")

		eh := &envoy_route_v3.HeaderMatcher{
			Name: ":method",
			HeaderMatchSpecifier: &envoy_route_v3.HeaderMatcher_SafeRegexMatch{
				SafeRegexMatch: makeEnvoyRegexMatch(methodHeaderRegex),
			},
		}

		envoyRouteMatch.Headers = append(envoyRouteMatch.Headers, eh)
	}

	if len(psRouteMatch.GetQueryParameterMatches()) > 0 {
		envoyRouteMatch.QueryParameters = make([]*envoy_route_v3.QueryParameterMatcher, 0, len(psRouteMatch.GetQueryParameterMatches()))
	}
	for _, psQM := range psRouteMatch.GetQueryParameterMatches() {
		envoyRouteMatch.QueryParameters = append(envoyRouteMatch.QueryParameters, makeEnvoyQueryParamFromProxystateQueryMatch(psQM))
	}

	return envoyRouteMatch
}

func makeEnvoyRegexMatch(pattern string) *envoy_matcher_v3.RegexMatcher {
	return &envoy_matcher_v3.RegexMatcher{
		EngineType: &envoy_matcher_v3.RegexMatcher_GoogleRe2{
			GoogleRe2: &envoy_matcher_v3.RegexMatcher_GoogleRE2{},
		},
		Regex: pattern,
	}
}

func makeEnvoyHeaderMatcherFromProxystateHeaderMatch(psMatch *pbproxystate.HeaderMatch) *envoy_route_v3.HeaderMatcher {
	envoyHeaderMatcher := &envoy_route_v3.HeaderMatcher{
		Name: psMatch.Name,
	}

	switch psMatch.Match.(type) {
	case *pbproxystate.HeaderMatch_Exact:
		envoyHeaderMatcher.HeaderMatchSpecifier = &envoy_route_v3.HeaderMatcher_ExactMatch{
			ExactMatch: psMatch.GetExact(),
		}
	case *pbproxystate.HeaderMatch_Regex:
		envoyHeaderMatcher.HeaderMatchSpecifier = &envoy_route_v3.HeaderMatcher_SafeRegexMatch{
			SafeRegexMatch: makeEnvoyRegexMatch(psMatch.GetRegex()),
		}
	case *pbproxystate.HeaderMatch_Prefix:
		envoyHeaderMatcher.HeaderMatchSpecifier = &envoy_route_v3.HeaderMatcher_PrefixMatch{
			PrefixMatch: psMatch.GetPrefix(),
		}
	case *pbproxystate.HeaderMatch_Suffix:
		envoyHeaderMatcher.HeaderMatchSpecifier = &envoy_route_v3.HeaderMatcher_SuffixMatch{
			SuffixMatch: psMatch.GetSuffix(),
		}
	case *pbproxystate.HeaderMatch_Present:
		envoyHeaderMatcher.HeaderMatchSpecifier = &envoy_route_v3.HeaderMatcher_PresentMatch{
			PresentMatch: true,
		}
	default:
		// This shouldn't be possible considering the types of HeaderMatch
		return nil
	}

	if psMatch.GetInvertMatch() {
		envoyHeaderMatcher.InvertMatch = true
	}

	return envoyHeaderMatcher
}

func makeEnvoyQueryParamFromProxystateQueryMatch(psMatch *pbproxystate.QueryParameterMatch) *envoy_route_v3.QueryParameterMatcher {
	envoyQueryParamMatcher := &envoy_route_v3.QueryParameterMatcher{
		Name: psMatch.Name,
	}

	switch psMatch.Match.(type) {
	case *pbproxystate.QueryParameterMatch_Exact:
		envoyQueryParamMatcher.QueryParameterMatchSpecifier = &envoy_route_v3.QueryParameterMatcher_StringMatch{
			StringMatch: &envoy_matcher_v3.StringMatcher{
				MatchPattern: &envoy_matcher_v3.StringMatcher_Exact{
					Exact: psMatch.GetExact(),
				},
			},
		}

	case *pbproxystate.QueryParameterMatch_Regex:
		envoyQueryParamMatcher.QueryParameterMatchSpecifier = &envoy_route_v3.QueryParameterMatcher_StringMatch{
			StringMatch: &envoy_matcher_v3.StringMatcher{
				MatchPattern: &envoy_matcher_v3.StringMatcher_SafeRegex{
					SafeRegex: makeEnvoyRegexMatch(psMatch.GetRegex()),
				},
			},
		}
	case *pbproxystate.QueryParameterMatch_Present:
		envoyQueryParamMatcher.QueryParameterMatchSpecifier = &envoy_route_v3.QueryParameterMatcher_PresentMatch{
			PresentMatch: true,
		}
	default:
		// This shouldn't be possible considering the types of QueryMatch
		return nil
	}

	return envoyQueryParamMatcher
}

// TODO (dans): Will this always be envoy_route_v3.Route_Route?
// Definitely for connect proxies this is the only option.
func (pr *ProxyResources) makeEnvoyRouteActionFromProxystateRouteDestination(psRouteDestination *pbproxystate.RouteDestination) *envoy_route_v3.Route_Route {
	envoyRouteRoute := &envoy_route_v3.Route_Route{
		Route: &envoy_route_v3.RouteAction{},
	}

	switch psRouteDestination.Destination.(type) {
	case *pbproxystate.RouteDestination_Cluster:
		psCluster := psRouteDestination.GetCluster()
		envoyRouteRoute.Route.ClusterSpecifier = &envoy_route_v3.RouteAction_Cluster{
			Cluster: psCluster.GetName(),
		}
		clusters, _ := pr.makeClusters(psCluster.Name)
		pr.envoyResources[xdscommon.ClusterType] = append(pr.envoyResources[xdscommon.ClusterType], clusters...)

	case *pbproxystate.RouteDestination_WeightedClusters:
		psWeightedClusters := psRouteDestination.GetWeightedClusters()
		envoyClusters := make([]*envoy_route_v3.WeightedCluster_ClusterWeight, 0, len(psWeightedClusters.GetClusters()))
		totalWeight := 0
		for _, psCluster := range psWeightedClusters.GetClusters() {
			clusters, _ := pr.makeClusters(psCluster.Name)
			pr.envoyResources[xdscommon.ClusterType] = append(pr.envoyResources[xdscommon.ClusterType], clusters...)
			totalWeight += int(psCluster.Weight.GetValue())
			envoyClusters = append(envoyClusters, makeEnvoyClusterWeightFromProxystateWeightedCluster(psCluster))
		}
		var envoyWeightScale *wrapperspb.UInt32Value
		if totalWeight == 10000 {
			envoyWeightScale = response.MakeUint32Value(10000)
		}

		envoyRouteRoute.Route.ClusterSpecifier = &envoy_route_v3.RouteAction_WeightedClusters{
			WeightedClusters: &envoy_route_v3.WeightedCluster{
				Clusters:    envoyClusters,
				TotalWeight: envoyWeightScale,
			},
		}
	default:
		// This shouldn't be possible considering the types of Destination
		return nil
	}

	injectEnvoyRouteActionWithProxystateDestinationConfig(envoyRouteRoute.Route, psRouteDestination.GetDestinationConfiguration())

	if psRouteDestination.GetDestinationConfiguration() != nil {
		config := psRouteDestination.GetDestinationConfiguration()
		action := envoyRouteRoute.Route

		action.PrefixRewrite = config.GetPrefixRewrite()

		if config.GetTimeoutConfig().GetTimeout() != nil {
			action.Timeout = config.GetTimeoutConfig().GetTimeout()
		}

		if config.GetTimeoutConfig().GetTimeout() != nil {
			action.Timeout = config.GetTimeoutConfig().GetTimeout()
		}

		if config.GetTimeoutConfig().GetIdleTimeout() != nil {
			action.IdleTimeout = config.GetTimeoutConfig().GetIdleTimeout()
		}

		if config.GetRetryPolicy() != nil {
			action.RetryPolicy = makeEnvoyRetryPolicyFromProxystateRetryPolicy(config.GetRetryPolicy())
		}
	}

	return envoyRouteRoute
}

func makeEnvoyClusterWeightFromProxystateWeightedCluster(cluster *pbproxystate.L7WeightedDestinationCluster) *envoy_route_v3.WeightedCluster_ClusterWeight {
	envoyClusterWeight := &envoy_route_v3.WeightedCluster_ClusterWeight{
		Name:   cluster.GetName(),
		Weight: cluster.GetWeight(),
	}

	for _, hm := range cluster.GetHeaderMutations() {
		injectEnvoyClusterWeightWithProxystateHeaderMutation(envoyClusterWeight, hm)
	}

	return envoyClusterWeight
}

func injectEnvoyClusterWeightWithProxystateHeaderMutation(envoyClusterWeight *envoy_route_v3.WeightedCluster_ClusterWeight, mutation *pbproxystate.HeaderMutation) {

	mutation.GetAction()
	switch mutation.GetAction().(type) {
	case *pbproxystate.HeaderMutation_RequestHeaderAdd:
		action := mutation.GetRequestHeaderAdd()
		header := action.GetHeader()
		app := action.GetAppendAction() == pbproxystate.AppendAction_APPEND_ACTION_APPEND_IF_EXISTS_OR_ADD

		hvo := &envoy_core_v3.HeaderValueOption{
			Header: &envoy_core_v3.HeaderValue{
				Key:   header.GetKey(),
				Value: header.GetValue(),
			},
			Append: response.MakeBoolValue(app),
		}
		envoyClusterWeight.RequestHeadersToAdd = append(envoyClusterWeight.RequestHeadersToAdd, hvo)

	case *pbproxystate.HeaderMutation_RequestHeaderRemove:
		action := mutation.GetRequestHeaderRemove()
		envoyClusterWeight.RequestHeadersToRemove = append(envoyClusterWeight.RequestHeadersToRemove, action.GetHeaderKeys()...)

	case *pbproxystate.HeaderMutation_ResponseHeaderAdd:
		action := mutation.GetResponseHeaderAdd()
		header := action.GetHeader()
		app := action.GetAppendAction() == pbproxystate.AppendAction_APPEND_ACTION_APPEND_IF_EXISTS_OR_ADD

		hvo := &envoy_core_v3.HeaderValueOption{
			Header: &envoy_core_v3.HeaderValue{
				Key:   header.GetKey(),
				Value: header.GetValue(),
			},
			Append: response.MakeBoolValue(app),
		}
		envoyClusterWeight.ResponseHeadersToAdd = append(envoyClusterWeight.ResponseHeadersToAdd, hvo)

	case *pbproxystate.HeaderMutation_ResponseHeaderRemove:
		action := mutation.GetResponseHeaderRemove()
		envoyClusterWeight.ResponseHeadersToRemove = append(envoyClusterWeight.ResponseHeadersToRemove, action.GetHeaderKeys()...)

	default:
		// This shouldn't be possible considering the types of Destination
		return
	}
}

func injectEnvoyRouteActionWithProxystateDestinationConfig(envoyAction *envoy_route_v3.RouteAction, config *pbproxystate.DestinationConfiguration) {
	if config == nil {
		return
	}

	if len(config.GetHashPolicies()) > 0 {
		envoyAction.HashPolicy = make([]*envoy_route_v3.RouteAction_HashPolicy, 0, len(config.GetHashPolicies()))
	}
	for _, policy := range config.GetHashPolicies() {
		envoyPolicy := makeEnvoyHashPolicyFromProxystateLBHashPolicy(policy)
		envoyAction.HashPolicy = append(envoyAction.HashPolicy, envoyPolicy)
	}

	if config.AutoHostRewrite != nil {
		envoyAction.HostRewriteSpecifier = &envoy_route_v3.RouteAction_AutoHostRewrite{
			AutoHostRewrite: config.AutoHostRewrite,
		}
	}
}

func makeEnvoyHashPolicyFromProxystateLBHashPolicy(psPolicy *pbproxystate.LoadBalancerHashPolicy) *envoy_route_v3.RouteAction_HashPolicy {

	switch psPolicy.GetPolicy().(type) {
	case *pbproxystate.LoadBalancerHashPolicy_ConnectionProperties:
		return &envoy_route_v3.RouteAction_HashPolicy{
			PolicySpecifier: &envoy_route_v3.RouteAction_HashPolicy_ConnectionProperties_{
				ConnectionProperties: &envoy_route_v3.RouteAction_HashPolicy_ConnectionProperties{
					SourceIp: true, // always true
				},
			},
			Terminal: psPolicy.GetConnectionProperties().GetTerminal(),
		}

	case *pbproxystate.LoadBalancerHashPolicy_Header:
		return &envoy_route_v3.RouteAction_HashPolicy{
			PolicySpecifier: &envoy_route_v3.RouteAction_HashPolicy_Header_{
				Header: &envoy_route_v3.RouteAction_HashPolicy_Header{
					HeaderName: psPolicy.GetHeader().GetName(),
				},
			},
			Terminal: psPolicy.GetHeader().GetTerminal(),
		}

	case *pbproxystate.LoadBalancerHashPolicy_Cookie:
		cookie := &envoy_route_v3.RouteAction_HashPolicy_Cookie{
			Name: psPolicy.GetCookie().GetName(),
			Path: psPolicy.GetCookie().GetPath(),
			Ttl:  psPolicy.GetCookie().GetTtl(),
		}

		return &envoy_route_v3.RouteAction_HashPolicy{
			PolicySpecifier: &envoy_route_v3.RouteAction_HashPolicy_Cookie_{
				Cookie: cookie,
			},
			Terminal: psPolicy.GetCookie().GetTerminal(),
		}

	case *pbproxystate.LoadBalancerHashPolicy_QueryParameter:
		return &envoy_route_v3.RouteAction_HashPolicy{
			PolicySpecifier: &envoy_route_v3.RouteAction_HashPolicy_QueryParameter_{
				QueryParameter: &envoy_route_v3.RouteAction_HashPolicy_QueryParameter{
					Name: psPolicy.GetQueryParameter().GetName(),
				},
			},
			Terminal: psPolicy.GetQueryParameter().GetTerminal(),
		}
	}
	// This shouldn't be possible considering the types of LoadBalancerPolicy
	return nil
}

func makeEnvoyRetryPolicyFromProxystateRetryPolicy(psRetryPolicy *pbproxystate.RetryPolicy) *envoy_route_v3.RetryPolicy {
	return &envoy_route_v3.RetryPolicy{
		NumRetries:           psRetryPolicy.GetNumRetries(),
		RetriableStatusCodes: psRetryPolicy.GetRetriableStatusCodes(),
		RetryOn:              psRetryPolicy.GetRetryOn(),
	}
}

func injectEnvoyRouteRuleWithProxystateHeaderMutation(envoyRouteRule *envoy_route_v3.Route, mutation *pbproxystate.HeaderMutation) {

	mutation.GetAction()
	switch mutation.GetAction().(type) {
	case *pbproxystate.HeaderMutation_RequestHeaderAdd:
		action := mutation.GetRequestHeaderAdd()
		header := action.GetHeader()
		app := action.GetAppendAction() == pbproxystate.AppendAction_APPEND_ACTION_APPEND_IF_EXISTS_OR_ADD

		hvo := &envoy_core_v3.HeaderValueOption{
			Header: &envoy_core_v3.HeaderValue{
				Key:   header.GetKey(),
				Value: header.GetValue(),
			},
			Append: response.MakeBoolValue(app),
		}
		envoyRouteRule.RequestHeadersToAdd = append(envoyRouteRule.RequestHeadersToAdd, hvo)

	case *pbproxystate.HeaderMutation_RequestHeaderRemove:
		action := mutation.GetRequestHeaderRemove()
		envoyRouteRule.RequestHeadersToRemove = append(envoyRouteRule.RequestHeadersToRemove, action.GetHeaderKeys()...)

	case *pbproxystate.HeaderMutation_ResponseHeaderAdd:
		action := mutation.GetResponseHeaderAdd()
		header := action.GetHeader()
		app := action.GetAppendAction() == pbproxystate.AppendAction_APPEND_ACTION_APPEND_IF_EXISTS_OR_ADD

		hvo := &envoy_core_v3.HeaderValueOption{
			Header: &envoy_core_v3.HeaderValue{
				Key:   header.GetKey(),
				Value: header.GetValue(),
			},
			Append: response.MakeBoolValue(app),
		}
		envoyRouteRule.ResponseHeadersToAdd = append(envoyRouteRule.ResponseHeadersToAdd, hvo)

	case *pbproxystate.HeaderMutation_ResponseHeaderRemove:
		action := mutation.GetResponseHeaderRemove()
		envoyRouteRule.ResponseHeadersToRemove = append(envoyRouteRule.ResponseHeadersToRemove, action.GetHeaderKeys()...)

	default:
		// This shouldn't be possible considering the types of Destination
		return
	}
}

func injectEnvoyVirtualHostWithProxystateHeaderMutation(envoyVirtualHost *envoy_route_v3.VirtualHost, mutation *pbproxystate.HeaderMutation) {

	mutation.GetAction()
	switch mutation.GetAction().(type) {
	case *pbproxystate.HeaderMutation_RequestHeaderAdd:
		action := mutation.GetRequestHeaderAdd()
		header := action.GetHeader()
		app := action.GetAppendAction() == pbproxystate.AppendAction_APPEND_ACTION_APPEND_IF_EXISTS_OR_ADD

		hvo := &envoy_core_v3.HeaderValueOption{
			Header: &envoy_core_v3.HeaderValue{
				Key:   header.GetKey(),
				Value: header.GetValue(),
			},
			Append: response.MakeBoolValue(app),
		}
		envoyVirtualHost.RequestHeadersToAdd = append(envoyVirtualHost.RequestHeadersToAdd, hvo)

	case *pbproxystate.HeaderMutation_RequestHeaderRemove:
		action := mutation.GetRequestHeaderRemove()
		envoyVirtualHost.RequestHeadersToRemove = append(envoyVirtualHost.RequestHeadersToRemove, action.GetHeaderKeys()...)

	case *pbproxystate.HeaderMutation_ResponseHeaderAdd:
		action := mutation.GetResponseHeaderAdd()
		header := action.GetHeader()
		app := action.GetAppendAction() == pbproxystate.AppendAction_APPEND_ACTION_APPEND_IF_EXISTS_OR_ADD

		hvo := &envoy_core_v3.HeaderValueOption{
			Header: &envoy_core_v3.HeaderValue{
				Key:   header.GetKey(),
				Value: header.GetValue(),
			},
			Append: response.MakeBoolValue(app),
		}
		envoyVirtualHost.ResponseHeadersToAdd = append(envoyVirtualHost.ResponseHeadersToAdd, hvo)

	case *pbproxystate.HeaderMutation_ResponseHeaderRemove:
		action := mutation.GetResponseHeaderRemove()
		envoyVirtualHost.ResponseHeadersToRemove = append(envoyVirtualHost.ResponseHeadersToRemove, action.GetHeaderKeys()...)

	default:
		// This shouldn't be possible considering the types of Destination
		return
	}
}
