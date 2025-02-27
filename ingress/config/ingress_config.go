// Copyright (c) 2022 Alibaba Group Holding Ltd.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package config

import (
	"encoding/json"
	"strings"
	"sync"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	wasm "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/wasm/v3"
	httppb "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	v3 "github.com/envoyproxy/go-control-plane/envoy/extensions/wasm/v3"
	"github.com/golang/protobuf/ptypes/wrappers"
	"google.golang.org/protobuf/types/known/anypb"
	networking "istio.io/api/networking/v1alpha3"
	"istio.io/istio/pilot/pkg/model"
	networkingutil "istio.io/istio/pilot/pkg/networking/util"
	"istio.io/istio/pilot/pkg/util/sets"
	"istio.io/istio/pkg/config"
	"istio.io/istio/pkg/config/constants"
	"istio.io/istio/pkg/config/schema/collection"
	"istio.io/istio/pkg/config/schema/gvk"
	"istio.io/istio/pkg/kube"
	listersv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"

	"github.com/alibaba/higress/ingress/kube/annotations"
	"github.com/alibaba/higress/ingress/kube/common"
	"github.com/alibaba/higress/ingress/kube/ingress"
	"github.com/alibaba/higress/ingress/kube/ingressv1"
	secretkube "github.com/alibaba/higress/ingress/kube/secret/kube"
	"github.com/alibaba/higress/ingress/kube/util"
	. "github.com/alibaba/higress/ingress/log"
)

var (
	_ model.ConfigStoreCache = &IngressConfig{}
	_ model.IngressStore     = &IngressConfig{}
)

type IngressConfig struct {
	// key: cluster id
	remoteIngressControllers map[string]common.IngressController
	mutex                    sync.RWMutex

	ingressRouteCache  model.IngressRouteCollection
	ingressDomainCache model.IngressDomainCollection

	localKubeClient kube.Client

	virtualServiceHandlers  []model.EventHandler
	gatewayHandlers         []model.EventHandler
	destinationRuleHandlers []model.EventHandler
	envoyFilterHandlers     []model.EventHandler
	watchErrorHandler       cache.WatchErrorHandler

	cachedEnvoyFilters []config.Config

	watchedSecretSet sets.Set

	XDSUpdater model.XDSUpdater

	annotationHandler annotations.AnnotationHandler

	globalGatewayName string

	namespace string

	clusterId string
}

func NewIngressConfig(localKubeClient kube.Client, XDSUpdater model.XDSUpdater, namespace, clusterId string) *IngressConfig {
	if clusterId == "Kubernetes" {
		clusterId = ""
	}
	return &IngressConfig{
		remoteIngressControllers: make(map[string]common.IngressController),
		localKubeClient:          localKubeClient,
		XDSUpdater:               XDSUpdater,
		annotationHandler:        annotations.NewAnnotationHandlerManager(),
		clusterId:                clusterId,
		globalGatewayName: namespace + "/" +
			common.CreateConvertedName(clusterId, "global"),
		watchedSecretSet: sets.NewSet(),
		namespace:        namespace,
	}
}

func (m *IngressConfig) RegisterEventHandler(kind config.GroupVersionKind, f model.EventHandler) {
	IngressLog.Infof("register resource %v", kind)
	if kind != gvk.VirtualService && kind != gvk.Gateway &&
		kind != gvk.DestinationRule && kind != gvk.EnvoyFilter {
		return
	}

	switch kind {
	case gvk.VirtualService:
		m.virtualServiceHandlers = append(m.virtualServiceHandlers, f)

	case gvk.Gateway:
		m.gatewayHandlers = append(m.gatewayHandlers, f)

	case gvk.DestinationRule:
		m.destinationRuleHandlers = append(m.destinationRuleHandlers, f)

	case gvk.EnvoyFilter:
		m.envoyFilterHandlers = append(m.envoyFilterHandlers, f)
	}

	for _, remoteIngressController := range m.remoteIngressControllers {
		remoteIngressController.RegisterEventHandler(kind, f)
	}
}

func (m *IngressConfig) AddLocalCluster(options common.Options) common.IngressController {
	secretController := secretkube.NewController(m.localKubeClient, options)
	secretController.AddEventHandler(m.ReflectSecretChanges)

	var ingressController common.IngressController
	v1 := common.V1Available(m.localKubeClient)
	if !v1 {
		ingressController = ingress.NewController(m.localKubeClient, m.localKubeClient, options, secretController)
	} else {
		ingressController = ingressv1.NewController(m.localKubeClient, m.localKubeClient, options, secretController)
	}

	m.remoteIngressControllers[options.ClusterId] = ingressController
	return ingressController
}

func (m *IngressConfig) InitializeCluster(ingressController common.IngressController, stop <-chan struct{}) error {
	for _, handler := range m.virtualServiceHandlers {
		ingressController.RegisterEventHandler(gvk.VirtualService, handler)
	}
	for _, handler := range m.gatewayHandlers {
		ingressController.RegisterEventHandler(gvk.Gateway, handler)
	}
	for _, handler := range m.destinationRuleHandlers {
		ingressController.RegisterEventHandler(gvk.DestinationRule, handler)
	}
	for _, handler := range m.envoyFilterHandlers {
		ingressController.RegisterEventHandler(gvk.EnvoyFilter, handler)
	}

	_ = ingressController.SetWatchErrorHandler(m.watchErrorHandler)

	go ingressController.Run(stop)
	return nil
}

func (m *IngressConfig) List(typ config.GroupVersionKind, namespace string) ([]config.Config, error) {
	if typ != gvk.Gateway &&
		typ != gvk.VirtualService &&
		typ != gvk.DestinationRule &&
		typ != gvk.EnvoyFilter {
		return nil, common.ErrUnsupportedOp
	}

	// Currently, only support list all namespaces gateways or virtualservices.
	if namespace != "" {
		IngressLog.Warnf("ingress store only support type %s of all namespace.", typ)
		return nil, common.ErrUnsupportedOp
	}

	if typ == gvk.EnvoyFilter {
		m.mutex.RLock()
		defer m.mutex.RUnlock()
		IngressLog.Infof("resource type %s, configs number %d", typ, len(m.cachedEnvoyFilters))
		return m.cachedEnvoyFilters, nil
	}

	var configs []config.Config
	m.mutex.RLock()
	for _, ingressController := range m.remoteIngressControllers {
		configs = append(configs, ingressController.List()...)
	}
	m.mutex.RUnlock()

	common.SortIngressByCreationTime(configs)
	wrapperConfigs := m.createWrapperConfigs(configs)

	IngressLog.Infof("resource type %s, configs number %d", typ, len(wrapperConfigs))
	switch typ {
	case gvk.Gateway:
		return m.convertGateways(wrapperConfigs), nil
	case gvk.VirtualService:
		return m.convertVirtualService(wrapperConfigs), nil
	case gvk.DestinationRule:
		return m.convertDestinationRule(wrapperConfigs), nil
	}

	return nil, nil
}

func (m *IngressConfig) createWrapperConfigs(configs []config.Config) []common.WrapperConfig {
	var wrapperConfigs []common.WrapperConfig

	// Init global context
	clusterSecretListers := map[string]listersv1.SecretLister{}
	clusterServiceListers := map[string]listersv1.ServiceLister{}
	m.mutex.RLock()
	for clusterId, controller := range m.remoteIngressControllers {
		clusterSecretListers[clusterId] = controller.SecretLister()
		clusterServiceListers[clusterId] = controller.ServiceLister()
	}
	m.mutex.RUnlock()
	globalContext := &annotations.GlobalContext{
		WatchedSecrets:      sets.NewSet(),
		ClusterSecretLister: clusterSecretListers,
		ClusterServiceList:  clusterServiceListers,
	}

	for idx := range configs {
		rawConfig := configs[idx]
		annotationsConfig := &annotations.Ingress{
			Meta: annotations.Meta{
				Namespace:    rawConfig.Namespace,
				Name:         rawConfig.Name,
				RawClusterId: common.GetRawClusterId(rawConfig.Annotations),
				ClusterId:    common.GetClusterId(rawConfig.Annotations),
			},
		}
		_ = m.annotationHandler.Parse(rawConfig.Annotations, annotationsConfig, globalContext)
		wrapperConfigs = append(wrapperConfigs, common.WrapperConfig{
			Config:            &rawConfig,
			AnnotationsConfig: annotationsConfig,
		})
	}

	m.mutex.Lock()
	m.watchedSecretSet = globalContext.WatchedSecrets
	m.mutex.Unlock()

	return wrapperConfigs
}

func (m *IngressConfig) convertGateways(configs []common.WrapperConfig) []config.Config {
	convertOptions := common.ConvertOptions{
		IngressDomainCache: common.NewIngressDomainCache(),
		Gateways:           map[string]*common.WrapperGateway{},
	}

	for idx := range configs {
		cfg := configs[idx]
		clusterId := common.GetClusterId(cfg.Config.Annotations)
		m.mutex.RLock()
		ingressController := m.remoteIngressControllers[clusterId]
		m.mutex.RUnlock()
		if ingressController == nil {
			continue
		}
		if err := ingressController.ConvertGateway(&convertOptions, &cfg); err != nil {
			IngressLog.Errorf("Convert ingress %s/%s to gateway fail in cluster %s, err %v", cfg.Config.Namespace, cfg.Config.Name, clusterId, err)
		}
	}

	// apply annotation
	for _, wrapperGateway := range convertOptions.Gateways {
		m.annotationHandler.ApplyGateway(wrapperGateway.Gateway, wrapperGateway.WrapperConfig.AnnotationsConfig)
	}

	m.mutex.Lock()
	m.ingressDomainCache = convertOptions.IngressDomainCache.Extract()
	m.mutex.Unlock()

	out := make([]config.Config, 0, len(convertOptions.Gateways))
	for _, gateway := range convertOptions.Gateways {
		cleanHost := common.CleanHost(gateway.Host)
		out = append(out, config.Config{
			Meta: config.Meta{
				GroupVersionKind: gvk.Gateway,
				Name:             common.CreateConvertedName(constants.IstioIngressGatewayName, cleanHost),
				Namespace:        m.namespace,
				Annotations: map[string]string{
					common.ClusterIdAnnotation: gateway.ClusterId,
					common.HostAnnotation:      gateway.Host,
				},
			},
			Spec: gateway.Gateway,
		})
	}
	return out
}

func (m *IngressConfig) convertVirtualService(configs []common.WrapperConfig) []config.Config {
	convertOptions := common.ConvertOptions{
		HostAndPath2Ingress: map[string]*config.Config{},
		IngressRouteCache:   common.NewIngressRouteCache(),
		VirtualServices:     map[string]*common.WrapperVirtualService{},
		HTTPRoutes:          map[string][]*common.WrapperHTTPRoute{},
	}

	// convert http route
	for idx := range configs {
		cfg := configs[idx]
		clusterId := common.GetClusterId(cfg.Config.Annotations)
		m.mutex.RLock()
		ingressController := m.remoteIngressControllers[clusterId]
		m.mutex.RUnlock()
		if ingressController == nil {
			continue
		}
		if err := ingressController.ConvertHTTPRoute(&convertOptions, &cfg); err != nil {
			IngressLog.Errorf("Convert ingress %s/%s to HTTP route fail in cluster %s, err %v", cfg.Config.Namespace, cfg.Config.Name, clusterId, err)
		}
	}

	// Apply annotation on routes
	for _, routes := range convertOptions.HTTPRoutes {
		for _, route := range routes {
			m.annotationHandler.ApplyRoute(route.HTTPRoute, route.WrapperConfig.AnnotationsConfig)
		}
	}

	// Apply canary ingress
	if len(configs) > len(convertOptions.CanaryIngresses) {
		m.applyCanaryIngresses(&convertOptions)
	}

	// Normalize weighted cluster to make sure the sum of weight is 100.
	for _, host := range convertOptions.HTTPRoutes {
		for _, route := range host {
			normalizeWeightedCluster(convertOptions.IngressRouteCache, route)
		}
	}

	// Apply spec default backend.
	if convertOptions.HasDefaultBackend {
		for idx := range configs {
			cfg := configs[idx]
			clusterId := common.GetClusterId(cfg.Config.Annotations)
			m.mutex.RLock()
			ingressController := m.remoteIngressControllers[clusterId]
			m.mutex.RUnlock()
			if ingressController == nil {
				continue
			}
			if err := ingressController.ApplyDefaultBackend(&convertOptions, &cfg); err != nil {
				IngressLog.Errorf("Apply default backend on ingress %s/%s fail in cluster %s, err %v", cfg.Config.Namespace, cfg.Config.Name, clusterId, err)
			}
		}
	}

	// Apply annotation on virtual services
	for _, virtualService := range convertOptions.VirtualServices {
		m.annotationHandler.ApplyVirtualServiceHandler(virtualService.VirtualService, virtualService.WrapperConfig.AnnotationsConfig)
	}

	// Apply app root for per host.
	m.applyAppRoot(&convertOptions)

	// Apply internal active redirect for error page.
	m.applyInternalActiveRedirect(&convertOptions)

	m.mutex.Lock()
	m.ingressRouteCache = convertOptions.IngressRouteCache.Extract()
	m.mutex.Unlock()

	// Convert http route to virtual service
	out := make([]config.Config, 0, len(convertOptions.HTTPRoutes))
	for host, routes := range convertOptions.HTTPRoutes {
		if len(routes) == 0 {
			continue
		}

		cleanHost := common.CleanHost(host)
		// namespace/name, name format: (istio cluster id)-host
		gateways := []string{m.namespace + "/" +
			common.CreateConvertedName(m.clusterId, cleanHost),
			common.CreateConvertedName(constants.IstioIngressGatewayName, cleanHost)}
		if host != "*" {
			gateways = append(gateways, m.globalGatewayName)
		}

		wrapperVS, exist := convertOptions.VirtualServices[host]
		if !exist {
			IngressLog.Warnf("virtual service for host %s does not exist.", host)
		}
		vs := wrapperVS.VirtualService
		vs.Gateways = gateways

		for _, route := range routes {
			vs.Http = append(vs.Http, route.HTTPRoute)
		}

		firstRoute := routes[0]
		out = append(out, config.Config{
			Meta: config.Meta{
				GroupVersionKind: gvk.VirtualService,
				Name:             common.CreateConvertedName(constants.IstioIngressGatewayName, firstRoute.WrapperConfig.Config.Namespace, firstRoute.WrapperConfig.Config.Name, cleanHost),
				Namespace:        m.namespace,
				Annotations: map[string]string{
					common.ClusterIdAnnotation: firstRoute.ClusterId,
				},
			},
			Spec: vs,
		})
	}

	// We generate some specific envoy filter here to avoid duplicated computation.
	m.convertEnvoyFilter(&convertOptions)

	return out
}

func (m *IngressConfig) convertEnvoyFilter(convertOptions *common.ConvertOptions) {
	var envoyFilters []config.Config
	mappings := map[string]*common.Rule{}

	for _, routes := range convertOptions.HTTPRoutes {
		for _, route := range routes {
			if strings.HasSuffix(route.HTTPRoute.Name, "app-root") {
				continue
			}

			auth := route.WrapperConfig.AnnotationsConfig.Auth
			if auth == nil {
				continue
			}

			key := auth.AuthSecret.String() + "/" + auth.AuthRealm
			if rule, exist := mappings[key]; !exist {
				mappings[key] = &common.Rule{
					Realm:       auth.AuthRealm,
					MatchRoute:  []string{route.HTTPRoute.Name},
					Credentials: auth.Credentials,
					Encrypted:   true,
				}
			} else {
				rule.MatchRoute = append(rule.MatchRoute, route.HTTPRoute.Name)
			}
		}
	}

	IngressLog.Infof("Found %d number of basic auth", len(mappings))
	if len(mappings) > 0 {
		rules := &common.BasicAuthRules{}
		for _, rule := range mappings {
			rules.Rules = append(rules.Rules, rule)
		}

		basicAuth, err := constructBasicAuthEnvoyFilter(rules, m.namespace)
		if err != nil {
			IngressLog.Errorf("Construct basic auth filter error %v", err)
		} else {
			envoyFilters = append(envoyFilters, *basicAuth)
		}
	}

	// TODO Support other envoy filters

	m.mutex.Lock()
	m.cachedEnvoyFilters = envoyFilters
	m.mutex.Unlock()
}

func (m *IngressConfig) convertDestinationRule(configs []common.WrapperConfig) []config.Config {
	convertOptions := common.ConvertOptions{
		Service2TrafficPolicy: map[common.ServiceKey]*common.WrapperTrafficPolicy{},
	}

	// Convert destination from service within ingress rule.
	for idx := range configs {
		cfg := configs[idx]
		clusterId := common.GetClusterId(cfg.Config.Annotations)
		m.mutex.RLock()
		ingressController := m.remoteIngressControllers[clusterId]
		m.mutex.RUnlock()
		if ingressController == nil {
			continue
		}
		if err := ingressController.ConvertTrafficPolicy(&convertOptions, &cfg); err != nil {
			IngressLog.Errorf("Convert ingress %s/%s to destination rule fail in cluster %s, err %v", cfg.Config.Namespace, cfg.Config.Name, clusterId, err)
		}
	}

	IngressLog.Debugf("traffic policy number %d", len(convertOptions.Service2TrafficPolicy))

	for _, wrapperTrafficPolicy := range convertOptions.Service2TrafficPolicy {
		m.annotationHandler.ApplyTrafficPolicy(wrapperTrafficPolicy.TrafficPolicy, wrapperTrafficPolicy.WrapperConfig.AnnotationsConfig)
	}

	// Merge multi-port traffic policy per service into one destination rule.
	destinationRules := map[string]*common.WrapperDestinationRule{}
	for key, wrapperTrafficPolicy := range convertOptions.Service2TrafficPolicy {
		serviceName := util.CreateServiceFQDN(key.Namespace, key.Name)
		dr, exist := destinationRules[serviceName]
		if !exist {
			dr = &common.WrapperDestinationRule{
				DestinationRule: &networking.DestinationRule{
					Host: serviceName,
					TrafficPolicy: &networking.TrafficPolicy{
						PortLevelSettings: []*networking.TrafficPolicy_PortTrafficPolicy{wrapperTrafficPolicy.TrafficPolicy},
					},
				},
				WrapperConfig: wrapperTrafficPolicy.WrapperConfig,
				ServiceKey:    key,
			}
		} else {
			dr.DestinationRule.TrafficPolicy.PortLevelSettings = append(dr.DestinationRule.TrafficPolicy.PortLevelSettings, wrapperTrafficPolicy.TrafficPolicy)
		}

		destinationRules[serviceName] = dr
	}

	out := make([]config.Config, 0, len(destinationRules))
	for _, dr := range destinationRules {
		drName := util.CreateDestinationRuleName(m.clusterId, dr.ServiceKey.Namespace, dr.ServiceKey.Name)
		out = append(out, config.Config{
			Meta: config.Meta{
				GroupVersionKind: gvk.DestinationRule,
				Name:             common.CreateConvertedName(constants.IstioIngressGatewayName, drName),
				Namespace:        m.namespace,
			},
			Spec: dr.DestinationRule,
		})
	}
	return out
}

func (m *IngressConfig) applyAppRoot(convertOptions *common.ConvertOptions) {
	for host, wrapVS := range convertOptions.VirtualServices {
		if wrapVS.AppRoot != "" {
			route := &common.WrapperHTTPRoute{
				HTTPRoute: &networking.HTTPRoute{
					Name: common.CreateConvertedName(host, "app-root"),
					Match: []*networking.HTTPMatchRequest{
						{
							Uri: &networking.StringMatch{
								MatchType: &networking.StringMatch_Exact{
									Exact: "/",
								},
							},
						},
					},
					Redirect: &networking.HTTPRedirect{
						RedirectCode: 302,
						Uri:          wrapVS.AppRoot,
					},
				},
				WrapperConfig: wrapVS.WrapperConfig,
				ClusterId:     wrapVS.WrapperConfig.AnnotationsConfig.ClusterId,
			}
			convertOptions.HTTPRoutes[host] = append([]*common.WrapperHTTPRoute{route}, convertOptions.HTTPRoutes[host]...)
		}
	}
}

func (m *IngressConfig) applyInternalActiveRedirect(convertOptions *common.ConvertOptions) {
	for host, routes := range convertOptions.HTTPRoutes {
		var tempRoutes []*common.WrapperHTTPRoute
		for _, route := range routes {
			tempRoutes = append(tempRoutes, route)
			if route.HTTPRoute.InternalActiveRedirect != nil {
				fallbackConfig := route.WrapperConfig.AnnotationsConfig.Fallback
				if fallbackConfig == nil {
					continue
				}

				typedNamespace := fallbackConfig.DefaultBackend
				internalRedirectRoute := route.HTTPRoute.DeepCopy()
				internalRedirectRoute.Name = internalRedirectRoute.Name + annotations.FallbackRouteNameSuffix
				internalRedirectRoute.InternalActiveRedirect = nil
				internalRedirectRoute.Match = []*networking.HTTPMatchRequest{
					{
						Uri: &networking.StringMatch{
							MatchType: &networking.StringMatch_Exact{
								Exact: "/",
							},
						},
						Headers: map[string]*networking.StringMatch{
							annotations.FallbackInjectHeaderRouteName: {
								MatchType: &networking.StringMatch_Exact{
									Exact: internalRedirectRoute.Name,
								},
							},
							annotations.FallbackInjectFallbackService: {
								MatchType: &networking.StringMatch_Exact{
									Exact: typedNamespace.String(),
								},
							},
						},
					},
				}
				internalRedirectRoute.Route = []*networking.HTTPRouteDestination{
					{
						Destination: &networking.Destination{
							Host: util.CreateServiceFQDN(typedNamespace.Namespace, typedNamespace.Name),
							Port: &networking.PortSelector{
								Number: fallbackConfig.Port,
							},
						},
						Weight: 100,
					},
				}

				tempRoutes = append([]*common.WrapperHTTPRoute{{
					HTTPRoute:     internalRedirectRoute,
					WrapperConfig: route.WrapperConfig,
					ClusterId:     route.ClusterId,
				}}, tempRoutes...)
			}
		}
		convertOptions.HTTPRoutes[host] = tempRoutes
	}
}

func (m *IngressConfig) ReflectSecretChanges(clusterNamespacedName util.ClusterNamespacedName) {
	var hit bool
	m.mutex.RLock()
	if m.watchedSecretSet.Contains(clusterNamespacedName.String()) {
		hit = true
	}
	m.mutex.RUnlock()

	if hit {
		push := func(kind config.GroupVersionKind) {
			m.XDSUpdater.ConfigUpdate(&model.PushRequest{
				Full: true,
				ConfigsUpdated: map[model.ConfigKey]struct{}{{
					Kind:      kind,
					Name:      clusterNamespacedName.Name,
					Namespace: clusterNamespacedName.Namespace,
				}: {}},
				Reason: []model.TriggerReason{"auth-secret-change"},
			})
		}
		push(gvk.VirtualService)
		push(gvk.EnvoyFilter)
	}
}

func normalizeWeightedCluster(cache *common.IngressRouteCache, route *common.WrapperHTTPRoute) {
	if len(route.HTTPRoute.Route) == 1 {
		route.HTTPRoute.Route[0].Weight = 100
		return
	}

	var weightTotal int32 = 0
	for idx, routeDestination := range route.HTTPRoute.Route {
		if idx == 0 {
			continue
		}

		weightTotal += routeDestination.Weight
	}

	if weightTotal < route.WeightTotal {
		weightTotal = route.WeightTotal
	}

	var sum int32
	for idx, routeDestination := range route.HTTPRoute.Route {
		if idx == 0 {
			continue
		}

		weight := float32(routeDestination.Weight) / float32(weightTotal)
		routeDestination.Weight = int32(weight * 100)

		sum += routeDestination.Weight
	}

	route.HTTPRoute.Route[0].Weight = 100 - sum

	// Update the recorded status in ingress builder
	if cache != nil {
		cache.Update(route)
	}
}

func (m *IngressConfig) applyCanaryIngresses(convertOptions *common.ConvertOptions) {
	if len(convertOptions.CanaryIngresses) == 0 {
		return
	}

	IngressLog.Infof("Found %d number of canary ingresses.", len(convertOptions.CanaryIngresses))
	for _, cfg := range convertOptions.CanaryIngresses {
		clusterId := common.GetClusterId(cfg.Config.Annotations)
		m.mutex.RLock()
		ingressController := m.remoteIngressControllers[clusterId]
		m.mutex.RUnlock()
		if ingressController == nil {
			continue
		}
		if err := ingressController.ApplyCanaryIngress(convertOptions, cfg); err != nil {
			IngressLog.Errorf("Apply canary ingress %s/%s fail in cluster %s, err %v", cfg.Config.Namespace, cfg.Config.Name, clusterId, err)
		}
	}
}

func constructBasicAuthEnvoyFilter(rules *common.BasicAuthRules, namespace string) (*config.Config, error) {
	rulesStr, err := json.Marshal(rules)
	if err != nil {
		return nil, err
	}
	configuration := &wrappers.StringValue{
		Value: string(rulesStr),
	}

	wasm := &wasm.Wasm{
		Config: &v3.PluginConfig{
			Name:     "basic-auth",
			FailOpen: true,
			Vm: &v3.PluginConfig_VmConfig{
				VmConfig: &v3.VmConfig{
					Runtime: "envoy.wasm.runtime.null",
					Code: &corev3.AsyncDataSource{
						Specifier: &corev3.AsyncDataSource_Local{
							Local: &corev3.DataSource{
								Specifier: &corev3.DataSource_InlineString{
									InlineString: "envoy.wasm.basic_auth",
								},
							},
						},
					},
				},
			},
			Configuration: networkingutil.MessageToAny(configuration),
		},
	}

	wasmAny, err := anypb.New(wasm)
	if err != nil {
		return nil, err
	}

	typedConfig := &httppb.HttpFilter{
		Name: "basic-auth",
		ConfigType: &httppb.HttpFilter_TypedConfig{
			TypedConfig: wasmAny,
		},
	}

	gogoTypedConfig, err := util.MessageToGoGoStruct(typedConfig)
	if err != nil {
		return nil, err
	}

	return &config.Config{
		Meta: config.Meta{
			GroupVersionKind: gvk.EnvoyFilter,
			Name:             common.CreateConvertedName(constants.IstioIngressGatewayName, "basic-auth"),
			Namespace:        namespace,
		},
		Spec: &networking.EnvoyFilter{
			ConfigPatches: []*networking.EnvoyFilter_EnvoyConfigObjectPatch{
				{
					ApplyTo: networking.EnvoyFilter_HTTP_FILTER,
					Match: &networking.EnvoyFilter_EnvoyConfigObjectMatch{
						Context: networking.EnvoyFilter_GATEWAY,
						ObjectTypes: &networking.EnvoyFilter_EnvoyConfigObjectMatch_Listener{
							Listener: &networking.EnvoyFilter_ListenerMatch{
								FilterChain: &networking.EnvoyFilter_ListenerMatch_FilterChainMatch{
									Filter: &networking.EnvoyFilter_ListenerMatch_FilterMatch{
										Name: "envoy.filters.network.http_connection_manager",
										SubFilter: &networking.EnvoyFilter_ListenerMatch_SubFilterMatch{
											Name: "envoy.filters.http.cors",
										},
									},
								},
							},
						},
					},
					Patch: &networking.EnvoyFilter_Patch{
						Operation: networking.EnvoyFilter_Patch_INSERT_AFTER,
						Value:     gogoTypedConfig,
					},
				},
			},
		},
	}, nil
}

func (m *IngressConfig) Run(<-chan struct{}) {}

func (m *IngressConfig) HasSynced() bool {
	m.mutex.RLock()
	defer m.mutex.RUnlock()
	for _, remoteIngressController := range m.remoteIngressControllers {
		if !remoteIngressController.HasSynced() {
			return false
		}
	}

	IngressLog.Info("Ingress config controller synced.")
	return true
}

func (m *IngressConfig) SetWatchErrorHandler(f func(r *cache.Reflector, err error)) error {
	m.watchErrorHandler = f
	return nil
}

func (m *IngressConfig) GetIngressRoutes() model.IngressRouteCollection {
	m.mutex.RLock()
	defer m.mutex.RUnlock()
	return m.ingressRouteCache
}

func (m *IngressConfig) GetIngressDomains() model.IngressDomainCollection {
	m.mutex.RLock()
	defer m.mutex.RUnlock()
	return m.ingressDomainCache
}

func (m *IngressConfig) Schemas() collection.Schemas {
	return common.Schemas
}

func (m *IngressConfig) Get(config.GroupVersionKind, string, string) *config.Config {
	return nil
}

func (m *IngressConfig) Create(config.Config) (revision string, err error) {
	return "", common.ErrUnsupportedOp
}

func (m *IngressConfig) Update(config.Config) (newRevision string, err error) {
	return "", common.ErrUnsupportedOp
}

func (m *IngressConfig) UpdateStatus(config.Config) (newRevision string, err error) {
	return "", common.ErrUnsupportedOp
}

func (m *IngressConfig) Patch(config.Config, config.PatchFunc) (string, error) {
	return "", common.ErrUnsupportedOp
}

func (m *IngressConfig) Delete(config.GroupVersionKind, string, string, *string) error {
	return common.ErrUnsupportedOp
}
