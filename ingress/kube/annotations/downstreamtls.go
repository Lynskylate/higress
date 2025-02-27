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

	networking "istio.io/api/networking/v1alpha3"
	"github.com/alibaba/higress/ingress/kube/util"
	. "github.com/alibaba/higress/ingress/log"
	"istio.io/istio/pilot/pkg/credentials/kube"
	"istio.io/istio/pilot/pkg/model"
	gatewaytool "istio.io/istio/pkg/config/gateway"
	"istio.io/istio/pkg/config/security"
)

const (
	authTLSSecret = "auth-tls-secret"
	tlsMinVersion = "tls-min-protocol-version"
	tlsMaxVersion = "tls-max-protocol-version"
	sslCipher     = "ssl-cipher"
)

type TLSProtocolVersion string

const (
	tlsV10 TLSProtocolVersion = "TLSv1.0"
	tlsV11 TLSProtocolVersion = "TLSv1.1"
	tlsV12 TLSProtocolVersion = "TLSv1.2"
	tlsV13 TLSProtocolVersion = "TLSv1.3"
)

var (
	_ Parser         = &downstreamTLS{}
	_ GatewayHandler = &downstreamTLS{}

	tlsProtocol = map[TLSProtocolVersion]networking.ServerTLSSettings_TLSProtocol{
		tlsV10: networking.ServerTLSSettings_TLSV1_0,
		tlsV11: networking.ServerTLSSettings_TLSV1_1,
		tlsV12: networking.ServerTLSSettings_TLSV1_2,
		tlsV13: networking.ServerTLSSettings_TLSV1_3,
	}
)

func isValidTLSProtocolVersion(protocol string) bool {
	tls := TLSProtocolVersion(protocol)
	_, exist := tlsProtocol[tls]
	return exist
}

func Convert(protocol string) networking.ServerTLSSettings_TLSProtocol {
	return tlsProtocol[TLSProtocolVersion(protocol)]
}

type DownstreamTLSConfig struct {
	TlsMinVersion TLSProtocolVersion
	TlsMaxVersion TLSProtocolVersion
	CipherSuites  []string
	Mode          networking.ServerTLSSettings_TLSmode
	CASecretName  model.NamespacedName
}

type downstreamTLS struct{}

func (d downstreamTLS) Parse(annotations Annotations, config *Ingress, _ *GlobalContext) error {
	if !needDownstreamTLS(annotations) {
		return nil
	}

	downstreamTLSConfig := &DownstreamTLSConfig{
		Mode: networking.ServerTLSSettings_SIMPLE,
	}
	defer func() {
		config.DownstreamTLS = downstreamTLSConfig
	}()

	if secretName, err := annotations.ParseStringASAP(authTLSSecret); err == nil {
		namespacedName := util.SplitNamespacedName(secretName)
		if namespacedName.Name == "" {
			IngressLog.Errorf("CA secret name %s format is invalid.", secretName)
		} else {
			if namespacedName.Namespace == "" {
				namespacedName.Namespace = config.Namespace
			}
			downstreamTLSConfig.CASecretName = namespacedName
			downstreamTLSConfig.Mode = networking.ServerTLSSettings_MUTUAL
		}
	}

	if minVersion, err := annotations.ParseStringForMSE(tlsMinVersion); err == nil &&
		isValidTLSProtocolVersion(minVersion) {
		downstreamTLSConfig.TlsMinVersion = TLSProtocolVersion(minVersion)
	}

	if maxVersion, err := annotations.ParseStringForMSE(tlsMaxVersion); err == nil &&
		isValidTLSProtocolVersion(maxVersion) {
		downstreamTLSConfig.TlsMaxVersion = TLSProtocolVersion(maxVersion)
	}

	if rawTlsCipherSuite, err := annotations.ParseStringASAP(sslCipher); err == nil {
		var validCipherSuite []string
		cipherList := strings.Split(rawTlsCipherSuite, ":")
		for _, cipher := range cipherList {
			if security.IsValidCipherSuite(cipher) {
				validCipherSuite = append(validCipherSuite, cipher)
			}
		}

		downstreamTLSConfig.CipherSuites = validCipherSuite
	}

	return nil
}

func (d downstreamTLS) ApplyGateway(gateway *networking.Gateway, config *Ingress) {
	if config.DownstreamTLS == nil {
		return
	}

	downstreamTLSConfig := config.DownstreamTLS
	for _, server := range gateway.Servers {
		if gatewaytool.IsTLSServer(server) {
			if downstreamTLSConfig.CASecretName.Name != "" {
				serverCert := extraSecret(server.Tls.CredentialName)
				if downstreamTLSConfig.CASecretName.Namespace != serverCert.Namespace ||
					(downstreamTLSConfig.CASecretName.Name != serverCert.Name &&
						downstreamTLSConfig.CASecretName.Name != serverCert.Name+kube.GatewaySdsCaSuffix) {
					IngressLog.Errorf("CA secret %s is invalid", downstreamTLSConfig.CASecretName.String())
				} else {
					server.Tls.Mode = downstreamTLSConfig.Mode
				}
			}

			if downstreamTLSConfig.TlsMinVersion != "" {
				server.Tls.MinProtocolVersion = tlsProtocol[downstreamTLSConfig.TlsMinVersion]
			}
			if downstreamTLSConfig.TlsMaxVersion != "" {
				server.Tls.MaxProtocolVersion = tlsProtocol[downstreamTLSConfig.TlsMaxVersion]
			}
			if len(downstreamTLSConfig.CipherSuites) != 0 {
				server.Tls.CipherSuites = downstreamTLSConfig.CipherSuites
			}
		}
	}
}

func needDownstreamTLS(annotations Annotations) bool {
	return annotations.HasMSE(tlsMinVersion) ||
		annotations.HasMSE(tlsMaxVersion) ||
		annotations.HasASAP(sslCipher) ||
		annotations.HasASAP(authTLSSecret)
}
