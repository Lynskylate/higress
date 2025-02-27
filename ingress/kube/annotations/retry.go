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
	"strings"

	"github.com/gogo/protobuf/types"

	networking "istio.io/api/networking/v1alpha3"
	"istio.io/istio/pilot/pkg/util/sets"
)

const (
	retryCount      = "proxy-next-upstream-tries"
	perRetryTimeout = "proxy-next-upstream-timeout"
	retryOn         = "proxy-next-upstream"

	defaultRetryCount = 3
	defaultRetryOn    = "5xx"
	retryStatusCode   = "retriable-status-codes"
)

var (
	_ Parser       = retry{}
	_ RouteHandler = retry{}
)

type RetryConfig struct {
	retryCount      int32
	perRetryTimeout *types.Duration
	retryOn         string
}

type retry struct{}

func (r retry) Parse(annotations Annotations, config *Ingress, _ *GlobalContext) error {
	if !needRetryConfig(annotations) {
		return nil
	}

	retryConfig := &RetryConfig{
		retryCount:      defaultRetryCount,
		perRetryTimeout: &types.Duration{},
		retryOn:         defaultRetryOn,
	}
	defer func() {
		config.Retry = retryConfig
	}()

	if count, err := annotations.ParseInt32ASAP(retryCount); err == nil {
		retryConfig.retryCount = count
	}

	if timeout, err := annotations.ParseIntASAP(perRetryTimeout); err == nil {
		retryConfig.perRetryTimeout = &types.Duration{
			Seconds: int64(timeout),
		}
	}

	if retryOn, err := annotations.ParseStringASAP(retryOn); err == nil {
		conditions := toSet(splitBySeparator(retryOn, ","))
		if len(conditions) > 0 {
			if conditions.Contains("off") {
				retryConfig.retryCount = 0
			} else {
				var stringBuilder strings.Builder
				// Convert error, timeout, invalid_header to 5xx
				if conditions.Contains("error") ||
					conditions.Contains("timeout") ||
					conditions.Contains("invalid_header") {
					stringBuilder.WriteString(defaultRetryOn + ",")
				}
				// Just use the raw.
				if conditions.Contains("non_idempotent") {
					stringBuilder.WriteString("non_idempotent,")
				}
				// Append the status codes.
				statusCodes := convertStatusCodes(conditions)
				if len(statusCodes) > 0 {
					stringBuilder.WriteString(retryStatusCode + ",")
					for _, code := range statusCodes {
						stringBuilder.WriteString(code + ",")
					}
				}

				retryConfig.retryOn = strings.TrimSuffix(stringBuilder.String(), ",")
			}
		}
	}

	return nil
}

func (r retry) ApplyRoute(route *networking.HTTPRoute, config *Ingress) {
	retryConfig := config.Retry
	if retryConfig == nil {
		return
	}

	route.Retries = &networking.HTTPRetry{
		Attempts:      retryConfig.retryCount,
		PerTryTimeout: retryConfig.perRetryTimeout,
		RetryOn:       retryConfig.retryOn,
	}
}

func needRetryConfig(annotations Annotations) bool {
	return annotations.HasASAP(retryCount) ||
		annotations.HasASAP(perRetryTimeout) ||
		annotations.HasASAP(retryOn)
}

func convertStatusCodes(set sets.Set) []string {
	var result []string
	for condition := range set {
		if strings.HasPrefix(condition, "http_") {
			result = append(result, strings.TrimPrefix(condition, "http_"))
		}
	}
	return result
}
