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
	"reflect"
	"testing"

	networking "istio.io/api/networking/v1alpha3"
)

func TestIPAccessControlParse(t *testing.T) {
	parser := ipAccessControl{}

	testCases := []struct {
		input  map[string]string
		expect *IPAccessControlConfig
	}{
		{},
		{
			input: map[string]string{
				DefaultAnnotationsPrefix + "/" + whitelist: "1.1.1.1",
				MSEAnnotationsPrefix + "/" + blacklist:     "2.2.2.2",
			},
			expect: &IPAccessControlConfig{
				Route: &IPAccessControl{
					isWhite:  true,
					remoteIp: []string{"1.1.1.1"},
				},
			},
		},
		{
			input: map[string]string{
				MSEAnnotationsPrefix + "/" + blacklist: "2.2.2.2",
			},
			expect: &IPAccessControlConfig{
				Route: &IPAccessControl{
					isWhite:  false,
					remoteIp: []string{"2.2.2.2"},
				},
			},
		},
		{
			input: map[string]string{
				MSEAnnotationsPrefix + "/" + domainWhitelist: "1.1.1.1",
			},
			expect: &IPAccessControlConfig{
				Domain: &IPAccessControl{
					isWhite:  true,
					remoteIp: []string{"1.1.1.1"},
				},
			},
		},
		{
			input: map[string]string{
				MSEAnnotationsPrefix + "/" + whitelist:       "1.1.1.1, 3.3.3.3",
				MSEAnnotationsPrefix + "/" + domainBlacklist: "2.2.2.2",
			},
			expect: &IPAccessControlConfig{
				Route: &IPAccessControl{
					isWhite:  true,
					remoteIp: []string{"1.1.1.1", "3.3.3.3"},
				},
				Domain: &IPAccessControl{
					isWhite:  false,
					remoteIp: []string{"2.2.2.2"},
				},
			},
		},
	}

	for _, testCase := range testCases {
		t.Run("", func(t *testing.T) {
			config := &Ingress{}
			_ = parser.Parse(testCase.input, config, nil)
			if !reflect.DeepEqual(testCase.expect, config.IPAccessControl) {
				t.Fatalf("Should be equal")
			}
		})
	}
}

func TestIpAccessControl_ApplyVirtualServiceHandler(t *testing.T) {
	parser := ipAccessControl{}

	testCases := []struct {
		config *Ingress
		input  *networking.VirtualService
		expect *networking.HTTPFilter
	}{
		{
			config: &Ingress{},
			input:  &networking.VirtualService{},
			expect: nil,
		},
		{
			config: &Ingress{
				IPAccessControl: &IPAccessControlConfig{
					Domain: &IPAccessControl{
						isWhite:  true,
						remoteIp: []string{"1.1.1.1"},
					},
				},
			},
			input: &networking.VirtualService{},
			expect: &networking.HTTPFilter{
				Name:    "ip-access-control",
				Disable: false,
				Filter: &networking.HTTPFilter_IpAccessControl{
					IpAccessControl: &networking.IPAccessControl{
						RemoteIpBlocks: []string{"1.1.1.1"},
					},
				},
			},
		},
		{
			config: &Ingress{
				IPAccessControl: &IPAccessControlConfig{
					Domain: &IPAccessControl{
						isWhite:  false,
						remoteIp: []string{"2.2.2.2"},
					},
				},
			},
			input: &networking.VirtualService{},
			expect: &networking.HTTPFilter{
				Name:    "ip-access-control",
				Disable: false,
				Filter: &networking.HTTPFilter_IpAccessControl{
					IpAccessControl: &networking.IPAccessControl{
						NotRemoteIpBlocks: []string{"2.2.2.2"},
					},
				},
			},
		},
	}

	for _, testCase := range testCases {
		t.Run("", func(t *testing.T) {
			parser.ApplyVirtualServiceHandler(testCase.input, testCase.config)
			if testCase.config.IPAccessControl == nil {
				if len(testCase.input.HostHTTPFilters) != 0 {
					t.Fatalf("Should be empty")
				}
			} else {
				if len(testCase.input.HostHTTPFilters) == 0 {
					t.Fatalf("Should be not empty")
				}
				if !reflect.DeepEqual(testCase.expect, testCase.input.HostHTTPFilters[0]) {
					t.Fatalf("Should be equal")
				}
			}
		})
	}
}

func TestIpAccessControl_ApplyRoute(t *testing.T) {
	parser := ipAccessControl{}

	testCases := []struct {
		config *Ingress
		input  *networking.HTTPRoute
		expect *networking.HTTPFilter
	}{
		{
			config: &Ingress{},
			input:  &networking.HTTPRoute{},
			expect: nil,
		},
		{
			config: &Ingress{
				IPAccessControl: &IPAccessControlConfig{
					Route: &IPAccessControl{
						isWhite:  true,
						remoteIp: []string{"1.1.1.1"},
					},
				},
			},
			input: &networking.HTTPRoute{},
			expect: &networking.HTTPFilter{
				Name:    "ip-access-control",
				Disable: false,
				Filter: &networking.HTTPFilter_IpAccessControl{
					IpAccessControl: &networking.IPAccessControl{
						RemoteIpBlocks: []string{"1.1.1.1"},
					},
				},
			},
		},
		{
			config: &Ingress{
				IPAccessControl: &IPAccessControlConfig{
					Route: &IPAccessControl{
						isWhite:  false,
						remoteIp: []string{"2.2.2.2"},
					},
				},
			},
			input: &networking.HTTPRoute{},
			expect: &networking.HTTPFilter{
				Name:    "ip-access-control",
				Disable: false,
				Filter: &networking.HTTPFilter_IpAccessControl{
					IpAccessControl: &networking.IPAccessControl{
						NotRemoteIpBlocks: []string{"2.2.2.2"},
					},
				},
			},
		},
	}

	for _, testCase := range testCases {
		t.Run("", func(t *testing.T) {
			parser.ApplyRoute(testCase.input, testCase.config)
			if testCase.config.IPAccessControl == nil {
				if len(testCase.input.RouteHTTPFilters) != 0 {
					t.Fatalf("Should be empty")
				}
			} else {
				if len(testCase.input.RouteHTTPFilters) == 0 {
					t.Fatalf("Should be not empty")
				}
				if !reflect.DeepEqual(testCase.expect, testCase.input.RouteHTTPFilters[0]) {
					t.Fatalf("Should be equal")
				}
			}
		})
	}
}
