package controller

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	monitoringv1alpha1 "github.com/patrickdk77/endpoint-monitoring-operator/api/v1alpha1"
	"github.com/patrickdk77/endpoint-monitoring-operator/pkg/factory"
)

type manifest struct {
	source string
	json   []byte
	obj    *unstructured.Unstructured
}

func manifests(source string, data []byte) []manifest {
	var out []manifest
	docs := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(data)))
	for {
		doc, err := docs.Read()
		if errors.Is(err, io.EOF) {
			return out
		}
		Expect(err).NotTo(HaveOccurred(), source)
		j, err := yaml.YAMLToJSON(doc)
		Expect(err).NotTo(HaveOccurred(), source)
		if string(j) == "null" {
			continue
		}
		obj := &unstructured.Unstructured{}
		Expect(obj.UnmarshalJSON(j)).To(Succeed(), source)
		out = append(out, manifest{source: source, json: j, obj: obj})
	}
}

func readManifests(path string) []manifest {
	data, err := os.ReadFile(path)
	Expect(err).NotTo(HaveOccurred())
	return manifests(path, data)
}

func validate(m manifest, secrets client.Client) error {
	obj := m.obj.DeepCopy()
	obj.SetNamespace("default")
	if err := k8sClient.Create(ctx, obj, client.DryRunAll, client.FieldValidation("Strict")); err != nil {
		return err
	}
	if m.obj.GetKind() != "EndpointMonitor" {
		return nil
	}
	var monitor monitoringv1alpha1.EndpointMonitor
	if err := json.Unmarshal(m.json, &monitor); err != nil {
		return err
	}
	namespace := monitor.Namespace
	if namespace == "" {
		namespace = "default"
	}
	if _, err := factory.NewDriver(monitor.Spec.Driver, monitor.Spec.Endpoint, &monitor, namespace, secrets); err != nil {
		return err
	}
	if _, err := factory.NewNotifier(&monitor.Spec.Notify); err != nil {
		return err
	}
	var refs []string
	if n := monitor.Spec.Notify.Email; n != nil && n.Enabled && n.EmailSecretRef.Name != "" {
		refs = append(refs, n.EmailSecretRef.Name)
	}
	if n := monitor.Spec.Notify.Webhook; n != nil && n.Enabled && n.Authorization.Name != "" {
		refs = append(refs, n.Authorization.Name)
	}
	if n := monitor.Spec.Notify.Valkey; n != nil && n.Enabled && n.SecretRef.Name != "" {
		refs = append(refs, n.SecretRef.Name)
	}
	for _, name := range refs {
		if err := secrets.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &corev1.Secret{}); err != nil {
			return err
		}
	}
	return nil
}

func (m manifest) problem(err error) string {
	return m.obj.GetKind() + " " + m.obj.GetName() + " in " + m.source + ": " + err.Error()
}

var _ = Describe("Examples", func() {
	var all []manifest
	var secrets client.Client

	BeforeEach(func() {
		files, err := filepath.Glob(filepath.Join("..", "..", "examples", "*.yaml"))
		Expect(err).NotTo(HaveOccurred())
		Expect(files).NotTo(BeEmpty())
		files = append(files, filepath.Join("..", "..", "config", "samples", "monitoring_v1alpha1_endpointmonitor.yaml"))
		all = nil
		builder := fake.NewClientBuilder()
		for _, file := range files {
			found := readManifests(file)
			Expect(found).NotTo(BeEmpty(), file)
			for _, m := range found {
				all = append(all, m)
				if m.obj.GetKind() == "Secret" {
					var secret corev1.Secret
					Expect(json.Unmarshal(m.json, &secret)).To(Succeed(), m.source)
					builder = builder.WithObjects(&secret)
				}
			}
		}
		secrets = builder.Build()
	})

	It("passes the CRD, builds its drivers and notifiers, and names each object once", func() {
		var problems []string
		seen := map[string]string{}
		for _, m := range all {
			key := m.obj.GetKind() + " " + m.obj.GetNamespace() + "/" + m.obj.GetName()
			if first, ok := seen[key]; ok {
				problems = append(problems, key+" in "+m.source+" is also in "+first)
			}
			seen[key] = m.source
			if err := validate(m, secrets); err != nil {
				problems = append(problems, m.problem(err))
			}
		}
		Expect(problems).To(BeEmpty())
	})

	It("matches the EndpointMonitors shown in the READMEs", func() {
		var problems []string
		block := regexp.MustCompile("(?s)```yaml\n(.*?)```")
		for _, name := range []string{"README.md", "README-INSTALL.md"} {
			path := filepath.Join("..", "..", name)
			data, err := os.ReadFile(path)
			Expect(err).NotTo(HaveOccurred())
			var monitors int
			for _, match := range block.FindAllSubmatch(data, -1) {
				if !strings.Contains(string(match[1]), "kind: EndpointMonitor") {
					continue
				}
				for _, m := range manifests(path, match[1]) {
					if err := validate(m, secrets); err != nil {
						problems = append(problems, m.problem(err))
					}
					monitors++
				}
			}
			Expect(monitors).NotTo(BeZero(), path)
		}
		Expect(problems).To(BeEmpty())
	})
})
