// Package storageclass holds the API strategy for StorageClasses.
package storageclass

import (
	metav1 "mini-k8s/pkg/apis/meta/v1"
	storagev1 "mini-k8s/pkg/apis/storage/v1"
	"mini-k8s/pkg/apis/storage/validation"
	"mini-k8s/pkg/apiserver"
)

// Strategy defaults and validates StorageClasses, whose provisioning
// settings are immutable.
var Strategy = apiserver.Strategy{
	Defaults: func(obj metav1.Object) { storagev1.SetDefaults(obj.(*storagev1.StorageClass)) },
	Validate: func(obj metav1.Object) []string {
		return validation.ValidateStorageClass(obj.(*storagev1.StorageClass))
	},
	ValidateUpdate: func(obj, old metav1.Object) []string {
		return validation.ValidateStorageClassUpdate(obj.(*storagev1.StorageClass), old.(*storagev1.StorageClass))
	},
}
