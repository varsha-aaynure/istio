//go:build integ
// +build integ

// Copyright Istio Authors
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

package scheck

import (
	"fmt"
	"net/http"
	"sort"
	"strings"

	echoClient "istio.io/istio/pkg/test/echo"
	"istio.io/istio/pkg/test/echo/check"
	"istio.io/istio/pkg/test/echo/common/scheme"
	"istio.io/istio/pkg/test/framework/components/echo"
)

func ReachedClusters(to echo.Instances, opts *echo.CallOptions) check.Checker {
	// TODO(https://github.com/istio/istio/issues/37307): Investigate why we don't reach all clusters.
	if to.Clusters().IsMulticluster() && opts.Count > 1 && opts.Scheme != scheme.GRPC && !opts.Target.Config().IsHeadless() {
		return check.ReachedClusters(to.Clusters())
	}
	return check.None()
}

func RBACFailure(opts *echo.CallOptions) check.Checker {
	if opts.PortName == "grpc" {
		return check.ErrorContains("rpc error: code = PermissionDenied desc = RBAC: access denied")
	}

	if strings.HasPrefix(opts.PortName, "tcp") {
		return check.ErrorContains("EOF")
	}

	return check.And(
		check.NoError(),
		check.Status(http.StatusForbidden))
}

func HeaderContains(hType echoClient.HeaderType, expected map[string][]string) check.Checker {
	return check.Each(func(r echoClient.Response) error {
		h := r.GetHeaders(hType)
		for _, key := range sortKeys(expected) {
			actual := h.Get(key)

			for _, value := range expected[key] {
				if !strings.Contains(actual, value) {
					return fmt.Errorf("status code %s, expected %s header `%s` to contain `%s`, value=`%s`, raw content=%s",
						r.Code, hType, key, value, actual, r.RawContent)
				}
			}
		}
		return nil
	})
}

func HeaderNotContains(hType echoClient.HeaderType, expected map[string][]string) check.Checker {
	return check.Each(func(r echoClient.Response) error {
		h := r.GetHeaders(hType)
		for _, key := range sortKeys(expected) {
			actual := h.Get(key)

			for _, value := range expected[key] {
				if strings.Contains(actual, value) {
					return fmt.Errorf("status code %s, expected %s header `%s` to not contain `%s`, value=`%s`, raw content=%s",
						r.Code, hType, key, value, actual, r.RawContent)
				}
			}
		}
		return nil
	})
}

func sortKeys(v map[string][]string) []string {
	out := make([]string, 0, len(v))
	for k := range v {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
