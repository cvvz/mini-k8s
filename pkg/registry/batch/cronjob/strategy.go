// Package cronjob holds the API strategy for CronJobs.
package cronjob

import (
	batchv1 "mini-k8s/pkg/apis/batch/v1"
	"mini-k8s/pkg/apis/batch/validation"
	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/apiserver"
)

// Strategy defaults and validates CronJobs.
var Strategy = apiserver.Strategy{
	Defaults: func(obj metav1.Object) { batchv1.SetDefaultsCronJob(obj.(*batchv1.CronJob)) },
	Validate: func(obj metav1.Object) []string { return validation.ValidateCronJob(obj.(*batchv1.CronJob)) },
}
