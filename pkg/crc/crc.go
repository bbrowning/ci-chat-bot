package crc

import (
	"bytes"
	"encoding/base64"

	crcv1alpha1 "github.com/bbrowning/crc-operator/pkg/apis/crc/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

func ClusterForConfig(bundleName string, pullSecret string, params map[string]string) (*crcv1alpha1.CrcCluster, error) {

	crc := &crcv1alpha1.CrcCluster{
		TypeMeta: metav1.TypeMeta{APIVersion: "crc.developer.openshift.io/v1alpha1", Kind: "CrcCluster"},
		Spec: crcv1alpha1.CrcClusterSpec{
			CPU:        10,
			Memory:     "18Gi",
			PullSecret: base64.StdEncoding.EncodeToString([]byte(pullSecret)),
			BundleName: bundleName,
		},
	}

	enableMonitoring := true
	for opt := range params {
		switch {
		case opt == "persistent":
			crc.Spec.Storage = crcv1alpha1.CrcStorageSpec{
				Persistent: true,
				Size:       "50Gi",
			}
		case opt == "no-monitoring":
			enableMonitoring = false
		default:
			// ignore
		}
	}
	crc.Spec.EnableMonitoring = &enableMonitoring
	crc = crc.DeepCopy()
	return crc, nil
}

func ObjectToUnstructured(obj runtime.Object) *unstructured.Unstructured {
	buf := &bytes.Buffer{}
	if err := unstructured.UnstructuredJSONScheme.Encode(obj, buf); err != nil {
		panic(err)
	}
	u := &unstructured.Unstructured{}
	if _, _, err := unstructured.UnstructuredJSONScheme.Decode(buf.Bytes(), nil, u); err != nil {
		panic(err)
	}
	return u
}

func UnstructuredToObject(in runtime.Unstructured, out runtime.Object) error {
	return runtime.DefaultUnstructuredConverter.FromUnstructured(in.UnstructuredContent(), out)
}
