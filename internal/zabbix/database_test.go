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

package zabbix

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	zabbixv1alpha1 "github.com/venkain/zabbix-k8s-operator/api/v1alpha1"
)

func TestDatabaseEnvVars(t *testing.T) {
	db := &DatabaseEnv{
		Host: "zabbix-db-rw", Port: "5432", User: "zabbix", Password: "secret", DBName: "zabbix",
	}

	serverWant := map[string]string{
		"DB_SERVER_HOST":    "zabbix-db-rw",
		"DB_SERVER_PORT":    "5432",
		"POSTGRES_USER":     "zabbix",
		"POSTGRES_PASSWORD": "secret",
		"POSTGRES_DB":       "zabbix",
	}
	for _, env := range db.ServerEnvVars() {
		want, ok := serverWant[env.Name]
		if !ok {
			t.Errorf("unexpected server env var %q", env.Name)
			continue
		}
		if env.Value != want {
			t.Errorf("server env %s = %q, want %q", env.Name, env.Value, want)
		}
		if env.ValueFrom != nil {
			t.Errorf("server env %s must be a plain value", env.Name)
		}
	}

	frontendWant := map[string]string{
		"DB_SERVER_HOST":    "zabbix-db-rw",
		"DB_SERVER_PORT":    "5432",
		"POSTGRES_USER":     "zabbix",
		"POSTGRES_PASSWORD": "secret",
		"POSTGRES_DB":       "zabbix",
	}
	for _, env := range db.FrontendEnvVars() {
		want, ok := frontendWant[env.Name]
		if !ok {
			t.Errorf("unexpected frontend env var %q", env.Name)
			continue
		}
		if env.Value != want {
			t.Errorf("frontend env %s = %q, want %q", env.Name, env.Value, want)
		}
	}
}

func TestResolveDatabase(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := zabbixv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	validSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "zabbix-db-app", Namespace: "test-ns"},
		Data: map[string][]byte{
			SecretKeyHost:     []byte("zabbix-db-rw"),
			SecretKeyPort:     []byte("5432"),
			SecretKeyUser:     []byte("zabbix"),
			SecretKeyPassword: []byte("secret"),
			SecretKeyDBName:   []byte("zabbix"),
		},
	}

	t.Run("cnpg reads the cluster app secret", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(validSecret).Build()
		spec := zabbixv1alpha1.DatabaseSpec{CNPG: &zabbixv1alpha1.CNPGDatabaseSpec{ClusterName: "zabbix-db"}}
		db, err := ResolveDatabase(context.Background(), c, "test-ns", spec)
		if err != nil {
			t.Fatalf("ResolveDatabase: %v", err)
		}
		if db.Host != "zabbix-db-rw" || db.Port != "5432" || db.User != "zabbix" || db.Password != "secret" || db.DBName != "zabbix" {
			t.Errorf("unexpected resolved database env: %+v", db)
		}
		if db.SecretName() != "zabbix-db-app" {
			t.Errorf("SecretName() = %q, want zabbix-db-app", db.SecretName())
		}
	})

	t.Run("external reads a user-provided secret", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(validSecret).Build()
		spec := zabbixv1alpha1.DatabaseSpec{External: &zabbixv1alpha1.ExternalDatabaseSpec{SecretName: "zabbix-db-app"}}
		if _, err := ResolveDatabase(context.Background(), c, "test-ns", spec); err != nil {
			t.Fatalf("ResolveDatabase: %v", err)
		}
	})

	t.Run("missing secret is reported", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(scheme).Build()
		spec := zabbixv1alpha1.DatabaseSpec{CNPG: &zabbixv1alpha1.CNPGDatabaseSpec{ClusterName: "zabbix-db"}}
		_, err := ResolveDatabase(context.Background(), c, "test-ns", spec)
		if err == nil || !strings.Contains(err.Error(), "not found") {
			t.Errorf("want not-found error, got %v", err)
		}
	})

	t.Run("missing keys are reported", func(t *testing.T) {
		partial := validSecret.DeepCopy()
		partial.Data = map[string][]byte{SecretKeyHost: []byte("zabbix-db-rw")}
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(partial).Build()
		spec := zabbixv1alpha1.DatabaseSpec{CNPG: &zabbixv1alpha1.CNPGDatabaseSpec{ClusterName: "zabbix-db"}}
		_, err := ResolveDatabase(context.Background(), c, "test-ns", spec)
		if err == nil || !strings.Contains(err.Error(), "missing keys") {
			t.Errorf("want missing-keys error, got %v", err)
		}
	})

	t.Run("no provider configured is rejected", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(scheme).Build()
		_, err := ResolveDatabase(context.Background(), c, "test-ns", zabbixv1alpha1.DatabaseSpec{})
		if err == nil || !strings.Contains(err.Error(), "exactly one") {
			t.Errorf("want exactly-one error, got %v", err)
		}
	})
}
