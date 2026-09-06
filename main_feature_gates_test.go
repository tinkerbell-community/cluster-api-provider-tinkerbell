/*
Copyright 2022 The Tinkerbell Authors.

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

package main

import (
	"flag"
	"io"
	"testing"
)

// The Cluster API operator renders a Provider CR's spec.manager.featureGates
// into a --feature-gates argument on the manager container. CAPT consumes no
// gates itself, but it must accept the standard Cluster API gate names so that
// setting them (uniformly across providers) does not crash the manager with
// "flag provided but not defined: -feature-gates".
func TestFeatureGatesFlagAcceptsStandardGates(t *testing.T) {
	t.Parallel()

	fs := flag.NewFlagSet("capt-test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	(&config{}).initFlags(fs)

	if err := fs.Parse([]string{"--feature-gates=ClusterTopology=true,MachinePool=true"}); err != nil {
		t.Fatalf("expected --feature-gates with standard Cluster API gates to parse, got: %v", err)
	}
}

func TestFeatureGatesFlagRejectsUnknownGate(t *testing.T) {
	t.Parallel()

	fs := flag.NewFlagSet("capt-test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	(&config{}).initFlags(fs)

	if err := fs.Parse([]string{"--feature-gates=NoSuchGate=true"}); err == nil {
		t.Fatal("expected an unknown feature gate to be rejected")
	}
}
