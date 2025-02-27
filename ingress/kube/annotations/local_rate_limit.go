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

package annotations

import (
	"github.com/gogo/protobuf/types"

	networking "istio.io/api/networking/v1alpha3"
	"istio.io/istio/pilot/pkg/networking/core/v1alpha3/mseingress"
)

const (
	limitRPM             = "route-limit-rpm"
	limitRPS             = "route-limit-rps"
	limitBurstMultiplier = "route-limit-burst-multiplier"

	defaultBurstMultiplier = 5
	defaultStatusCode      = 503
)

var (
	_ Parser       = localRateLimit{}
	_ RouteHandler = localRateLimit{}

	second = &types.Duration{
		Seconds: 1,
	}

	minute = &types.Duration{
		Seconds: 60,
	}
)

type localRateLimitConfig struct {
	TokensPerFill uint32
	MaxTokens     uint32
	FillInterval  *types.Duration
}

type localRateLimit struct{}

func (l localRateLimit) Parse(annotations Annotations, config *Ingress, _ *GlobalContext) error {
	if !needLocalRateLimitConfig(annotations) {
		return nil
	}

	var local *localRateLimitConfig
	defer func() {
		config.localRateLimit = local
	}()

	var multiplier uint32 = defaultBurstMultiplier
	if m, err := annotations.ParseUint32ForMSE(limitBurstMultiplier); err == nil {
		multiplier = m
	}

	if rpm, err := annotations.ParseUint32ForMSE(limitRPM); err == nil {
		local = &localRateLimitConfig{
			MaxTokens:     rpm * multiplier,
			TokensPerFill: rpm,
			FillInterval:  minute,
		}
	} else if rps, err := annotations.ParseUint32ForMSE(limitRPS); err == nil {
		local = &localRateLimitConfig{
			MaxTokens:     rps * multiplier,
			TokensPerFill: rps,
			FillInterval:  second,
		}
	}

	return nil
}

func (l localRateLimit) ApplyRoute(route *networking.HTTPRoute, config *Ingress) {
	localRateLimitConfig := config.localRateLimit
	if localRateLimitConfig == nil {
		return
	}

	route.RouteHTTPFilters = append(route.RouteHTTPFilters, &networking.HTTPFilter{
		Name: mseingress.LocalRateLimit,
		Filter: &networking.HTTPFilter_LocalRateLimit{
			LocalRateLimit: &networking.LocalRateLimit{
				TokenBucket: &networking.TokenBucket{
					MaxTokens:     localRateLimitConfig.MaxTokens,
					TokensPefFill: localRateLimitConfig.TokensPerFill,
					FillInterval:  localRateLimitConfig.FillInterval,
				},
				StatusCode: defaultStatusCode,
			},
		},
	})
}

func needLocalRateLimitConfig(annotations Annotations) bool {
	return annotations.HasMSE(limitRPM) ||
		annotations.HasMSE(limitRPS)
}
