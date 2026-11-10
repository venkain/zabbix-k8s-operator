/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package zabbix contains the resource builders and convergence helpers
// used by the Zabbix controllers. All object construction is pure (no I/O)
// so builders are trivially unit-testable; the controller applies them via
// CreateOrUpdate.
package zabbix

import (
	"regexp"

	zabbixv1alpha1 "github.com/venkain/zabbix-k8s-operator/api/v1alpha1"
)

const (
	// OperatorName is the value of the managed-by label on every object
	// the operator creates.
	OperatorName = "zabbix-k8s-operator"

	// ComponentLabel identifies the Zabbix component an object belongs to.
	ComponentLabel = "zabbix.venkain.io/component"

	// InstanceLabel identifies the Zabbix (or ZabbixProxyGroup) resource
	// an object was created for.
	InstanceLabel = "zabbix.venkain.io/instance"

	// PartOfLabel ties objects to the application family.
	PartOfLabel = "app.kubernetes.io/part-of"

	// ManagedByLabel follows the recommended labels convention.
	ManagedByLabel = "app.kubernetes.io/managed-by"

	// NameLabel follows the recommended labels convention.
	NameLabel = "app.kubernetes.io/name"

	// HARoleLabel marks the active server HA node. It is maintained on
	// server pods (and drives the active-node Service selector) so that
	// the frontend and proxies reach the active node only.
	HARoleLabel = "zabbix.venkain.io/server-ha-role"

	// HARoleActive is the value of HARoleLabel on the active node.
	HARoleActive = "active"

	// Component values.
	ComponentServer     = "server"
	ComponentFrontend   = "frontend"
	ComponentWebService = "web-service"
	ComponentProxy      = "proxy"

	// Container ports (official images).
	ServerPort   = int32(10051)
	FrontendPort = int32(8080) // nginx in the official image listens on 8080
	// frontendServicePort is the port the frontend Service exposes
	// (targets FrontendPort); the API client routes through it.
	frontendServicePort = int32(80)
	WebServicePort      = int32(10053)
	// ProxyPort is the proxy daemon's ListenPort (trapper/passive agent
	// traffic); the same port is registered as each group member's
	// local_port.
	ProxyPort = int32(10051)
)

// Names of the derived objects for a Zabbix resource named <name>.
const (
	resourceSuffixServer     = "-server"
	resourceSuffixFrontend   = "-frontend"
	resourceSuffixWebService = "-web-service"
	resourceSuffixProxy      = "-proxy"
)

// managedProxyRecordPattern matches the server-side proxy records the
// operator creates for a group (<pg.Name>-proxy-<ordinal>); records not
// matching the pattern belong to someone else and are never touched.
var managedProxyRecordPattern = regexp.MustCompile(`-proxy-[0-9]+$`)

// IsManagedProxyRecord reports whether the given server-side proxy
// record name belongs to the instance's own members.
func IsManagedProxyRecord(instance, host string) bool {
	return len(host) > len(instance) &&
		host[:len(instance)] == instance &&
		managedProxyRecordPattern.MatchString(host[len(instance):])
}

// ServerName returns the name for the server's derived objects.
func ServerName(instance string) string { return instance + resourceSuffixServer }

// FrontendName returns the name for the frontend's derived objects.
func FrontendName(instance string) string { return instance + resourceSuffixFrontend }

// WebServiceName returns the name for the web service's derived objects.
func WebServiceName(instance string) string { return instance + resourceSuffixWebService }

// ProxyName returns the name for a proxy group's derived objects (the
// headless Service fronting the member pods and the StatefulSet
// serviceName). The pod names double as the Zabbix proxy identities: each
// group member registers with the server under <instance>-proxy-<ordinal>.
func ProxyName(instance string) string { return instance + resourceSuffixProxy }

// ImageFor returns the official container image for a component,
// honoring user overrides. Tag derivation from spec.version:
//   - "8.0" (release line)      → ubuntu-8.0-latest
//   - "8.0.2" (point release)   → ubuntu-8.0.2
func ImageFor(component, version string, override *zabbixv1alpha1.ImageSpec) string {
	repo := "zabbix/zabbix-" + component
	tag := "ubuntu-" + version + "-latest"
	if isPointRelease(version) {
		tag = "ubuntu-" + version
	}
	if override != nil {
		if override.Repository != "" {
			repo = override.Repository
		}
		if override.Tag != "" {
			tag = override.Tag
		}
	}
	return repo + ":" + tag
}

// isPointRelease reports whether the version string names a specific
// point release ("8.0.2") rather than a release line ("8.0").
func isPointRelease(version string) bool {
	dots := 0
	for _, r := range version {
		if r == '.' {
			dots++
		}
	}
	return dots > 1
}

// commonLabels returns the label set shared by all objects of an instance.
func commonLabels(instance, component string) map[string]string {
	return map[string]string{
		ComponentLabel: component,
		InstanceLabel:  instance,
		NameLabel:      component,
		PartOfLabel:    OperatorName,
		ManagedByLabel: OperatorName,
	}
}

// selectorLabels is the minimal, immutable label subset used in match
// selectors (never extended after creation — selector immutability is a
// hard Kubernetes rule).
func selectorLabels(instance, component string) map[string]string {
	return map[string]string{
		ComponentLabel: component,
		InstanceLabel:  instance,
	}
}
