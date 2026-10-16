package resources

import "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

func unstructuredInt(o map[string]any, f ...string) (int64, bool, error) {
	return unstructured.NestedInt64(o, f...)
}
