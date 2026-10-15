package validation

import (
	"encoding/json"
	"fmt"
	"maps"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"

	corev1 "mini-k8s/pkg/apis/core/v1"
	"mini-k8s/pkg/fieldpath"
	"mini-k8s/pkg/util/validation"
)

// Messages and field sets shared with Kubernetes.
const (
	fileModeMessage   = "must be a number between 0 and 0777 (octal), both inclusive"
	immutableMessage  = "field is immutable when `immutable` is set"
	conflictingPaths  = "conflicting duplicate paths"
	cpuDivisorMessage = "only divisor's values 1m and 1 are supported with the cpu resource"
	memDivisorMessage = "only divisor's values 1, 1k, 1M, 1G, 1T, 1P, 1E, 1Ki, 1Mi, 1Gi, 1Ti, 1Pi, 1Ei are supported with the memory resource"
	ephDivisorMessage = "only divisor's values 1, 1k, 1M, 1G, 1T, 1P, 1E, 1Ki, 1Mi, 1Gi, 1Ti, 1Pi, 1Ei are supported with the local ephemeral storage resource"
)

var (
	envFieldPaths = []string{"metadata.name", "metadata.namespace", "metadata.uid", "spec.nodeName", "spec.serviceAccountName",
		"status.hostIP", "status.hostIPs", "status.podIP", "status.podIPs"}
	volumeFieldPaths = []string{"metadata.annotations", "metadata.labels", "metadata.name", "metadata.namespace", "metadata.uid"}
	resourceFields   = []string{"limits.cpu", "limits.ephemeral-storage", "limits.memory", "requests.cpu", "requests.ephemeral-storage", "requests.memory"}
	cpuDivisors      = []string{"1m", "1"}
	byteDivisors     = []string{"1", "1k", "1M", "1G", "1T", "1P", "1E", "1Ki", "1Mi", "1Gi", "1Ti", "1Pi", "1Ei"}
)

func invalid(path string, value any, msg string) string {
	return fmt.Sprintf("%s: Invalid value: %s: %s", path, quote(value), msg)
}

func quote(v any) string {
	if s, ok := v.(string); ok {
		return fmt.Sprintf("%q", s)
	}
	return fmt.Sprint(v)
}

func notSupported(path, value string, supported []string) string {
	quoted := make([]string, len(supported))
	for i, s := range supported {
		quoted[i] = fmt.Sprintf("%q", s)
	}
	return fmt.Sprintf("%s: Unsupported value: %q: supported values: %s", path, value, strings.Join(quoted, ", "))
}

func validateConfigKeys(errs []string, path string, keys []string) []string {
	sort.Strings(keys)
	for _, k := range keys {
		for _, msg := range validation.IsConfigMapKey(k) {
			errs = append(errs, invalid(fmt.Sprintf("%s[%s]", path, k), k, msg))
		}
	}
	return errs
}

// ValidateConfigMap checks the keys and size of a ConfigMap.
func ValidateConfigMap(cm *corev1.ConfigMap) []string {
	errs := validateConfigKeys(nil, "data", slices.Collect(maps.Keys(cm.Data)))
	errs = validateConfigKeys(errs, "binaryData", slices.Collect(maps.Keys(cm.BinaryData)))
	size := 0
	for _, k := range slices.Sorted(maps.Keys(cm.Data)) {
		size += len(cm.Data[k])
		if _, ok := cm.BinaryData[k]; ok {
			errs = append(errs, invalid(fmt.Sprintf("data[%s]", k), k, "duplicate of key present in binaryData"))
		}
	}
	for _, v := range cm.BinaryData {
		size += len(v)
	}
	if size > corev1.MaxConfigDataSize {
		errs = append(errs, fmt.Sprintf("data: Too long: must have at most %d bytes", corev1.MaxConfigDataSize))
	}
	return errs
}

// validateImmutable forbids changing the data of an immutable object and
// making it mutable again.
func validateImmutable(oldImmutable, newImmutable *bool, sameData bool) []string {
	if oldImmutable == nil || !*oldImmutable {
		return nil
	}
	var errs []string
	if !sameData {
		errs = append(errs, "data: Forbidden: "+immutableMessage)
	}
	if newImmutable == nil || !*newImmutable {
		errs = append(errs, "immutable: Forbidden: "+immutableMessage)
	}
	return errs
}

// ValidateConfigMapUpdate enforces immutable ConfigMaps.
func ValidateConfigMapUpdate(cm, old *corev1.ConfigMap) []string {
	same := reflect.DeepEqual(cm.Data, old.Data) && reflect.DeepEqual(cm.BinaryData, old.BinaryData)
	return validateImmutable(old.Immutable, cm.Immutable, same)
}

// ValidateSecret checks the keys and size of a Secret and the keys its type
// requires (after stringData was merged into data).
func ValidateSecret(s *corev1.Secret) []string {
	errs := validateConfigKeys(nil, "data", slices.Collect(maps.Keys(s.Data)))
	size := 0
	for _, v := range s.Data {
		size += len(v)
	}
	if size > corev1.MaxConfigDataSize {
		errs = append(errs, fmt.Sprintf("data: Too long: must have at most %d bytes", corev1.MaxConfigDataSize))
	}
	required := func(keys ...string) {
		for _, k := range keys {
			if _, ok := s.Data[k]; !ok {
				errs = append(errs, fmt.Sprintf("data[%s]: Required value", k))
			}
		}
	}
	switch s.Type {
	case corev1.SecretTypeTLS:
		required(corev1.TLSCertKey, corev1.TLSPrivateKeyKey)
	case corev1.SecretTypeBasicAuth:
		_, user := s.Data[corev1.BasicAuthUsernameKey]
		_, pass := s.Data[corev1.BasicAuthPasswordKey]
		if !user && !pass {
			required(corev1.BasicAuthUsernameKey, corev1.BasicAuthPasswordKey)
		}
	case corev1.SecretTypeSSHAuth:
		required(corev1.SSHAuthPrivateKey)
	case corev1.SecretTypeDockerConfigJSON:
		if data, ok := s.Data[corev1.DockerConfigJSONKey]; !ok {
			required(corev1.DockerConfigJSONKey)
		} else if err := json.Unmarshal(data, &map[string]any{}); err != nil {
			errs = append(errs, invalid("data["+corev1.DockerConfigJSONKey+"]", "<secret contents redacted>", err.Error()))
		}
	}
	return errs
}

// ValidateSecretUpdate keeps the type and enforces immutable Secrets.
func ValidateSecretUpdate(s, old *corev1.Secret) []string {
	var errs []string
	if s.Type != old.Type {
		errs = append(errs, invalid("type", s.Type, "field is immutable"))
	}
	return append(errs, validateImmutable(old.Immutable, s.Immutable, reflect.DeepEqual(s.Data, old.Data))...)
}

// validateEnv checks a container's env and envFrom.
func validateEnv(path string, c corev1.Container) []string {
	var errs []string
	for i, e := range c.Env {
		p := fmt.Sprintf("%s.env[%d]", path, i)
		if e.Name == "" {
			errs = append(errs, p+".name: Required value")
		} else {
			for _, msg := range validation.IsRelaxedEnvVarName(e.Name) {
				errs = append(errs, invalid(p+".name", e.Name, msg))
			}
		}
		errs = append(errs, validateEnvVarSource(p+".valueFrom", e)...)
	}
	for i, from := range c.EnvFrom {
		p := fmt.Sprintf("%s.envFrom[%d]", path, i)
		if from.Prefix != "" {
			for _, msg := range validation.IsRelaxedEnvVarName(from.Prefix) {
				errs = append(errs, invalid(p+".prefix", from.Prefix, msg))
			}
		}
		sources := 0
		if from.ConfigMapRef != nil {
			sources++
			errs = append(errs, validateObjectName(p+".configMapRef.name", from.ConfigMapRef.Name, true)...)
		}
		if from.SecretRef != nil {
			sources++
			errs = append(errs, validateObjectName(p+".secretRef.name", from.SecretRef.Name, true)...)
		}
		switch {
		case sources == 0:
			errs = append(errs, invalid(path+".envFrom", "", "must specify one of: `configMapRef` or `secretRef`"))
		case sources > 1:
			errs = append(errs, invalid(path+".envFrom", "", "may not have more than one field specified at a time"))
		}
	}
	return errs
}

// validateObjectName checks the name of a referenced ConfigMap or Secret;
// envFrom reports an empty one as required, key selectors as invalid.
func validateObjectName(path, name string, emptyIsRequired bool) []string {
	if name == "" && emptyIsRequired {
		return []string{path + ": Required value"}
	}
	if !validation.IsDNS1123Subdomain(name) {
		return []string{invalid(path, name, validation.DNS1123SubdomainMessage)}
	}
	return nil
}

func validateKeySelector(path, name, key string) []string {
	errs := validateObjectName(path+".name", name, false)
	if key == "" {
		return append(errs, path+".key: Required value")
	}
	for _, msg := range validation.IsConfigMapKey(key) {
		errs = append(errs, invalid(path+".key", key, msg))
	}
	return errs
}

func validateEnvVarSource(path string, e corev1.EnvVar) []string {
	from := e.ValueFrom
	if from == nil {
		return nil
	}
	var errs []string
	sources := 0
	if from.FieldRef != nil {
		sources++
		errs = append(errs, validateFieldRef(path+".fieldRef", from.FieldRef, envFieldPaths)...)
	}
	if from.ResourceFieldRef != nil {
		sources++
		errs = append(errs, validateResourceFieldRef(path+".resourceFieldRef", from.ResourceFieldRef, false)...)
	}
	if r := from.ConfigMapKeyRef; r != nil {
		sources++
		errs = append(errs, validateKeySelector(path+".configMapKeyRef", r.Name, r.Key)...)
	}
	if r := from.SecretKeyRef; r != nil {
		sources++
		errs = append(errs, validateKeySelector(path+".secretKeyRef", r.Name, r.Key)...)
	}
	switch {
	case sources == 0:
		errs = append(errs, invalid(path, "", "must specify one of: `fieldRef`, `resourceFieldRef`, `configMapKeyRef` or `secretKeyRef`"))
	case e.Value != "":
		errs = append(errs, invalid(path, "", "may not be specified when `value` is not empty"))
	case sources > 1:
		errs = append(errs, invalid(path, "", "may not have more than one field specified at a time"))
	}
	return errs
}

// validateFieldRef checks a downward API field path against the fields
// allowed in this place; labels and annotations may also be subscripted.
func validateFieldRef(path string, f *corev1.ObjectFieldSelector, allowed []string) []string {
	if f.APIVersion != "" && f.APIVersion != "v1" {
		return []string{invalid(path+".apiVersion", f.APIVersion, "unsupported API version")}
	}
	if f.FieldPath == "" {
		return []string{path + ".fieldPath: Required value"}
	}
	field, subscript, ok := fieldpath.SplitMaybeSubscriptedPath(f.FieldPath)
	if !fieldpath.IsPodField(field) {
		return []string{invalid(path+".fieldPath", f.FieldPath, "error converting fieldPath: field label not supported: "+f.FieldPath)}
	}
	if ok {
		if field != "metadata.labels" && field != "metadata.annotations" {
			return []string{invalid(path+".fieldPath", field, "does not support subscript")}
		}
		if subscript == "" {
			return []string{invalid(path+".fieldPath", subscript, "must be a qualified name")}
		}
		return nil
	}
	if !slices.Contains(allowed, field) {
		return []string{notSupported(path+".fieldPath", field, allowed)}
	}
	return nil
}

func validateResourceFieldRef(path string, r *corev1.ResourceFieldSelector, volume bool) []string {
	var errs []string
	if volume && r.ContainerName == "" {
		errs = append(errs, path+".containerName: Required value")
	}
	if r.Resource == "" {
		errs = append(errs, path+".resource: Required value")
	} else if !slices.Contains(resourceFields, r.Resource) {
		errs = append(errs, notSupported(path+".resource", r.Resource, resourceFields))
	}
	if r.Divisor == nil || r.Divisor.IsZero() {
		return errs
	}
	d := r.Divisor.String()
	switch r.Resource {
	case "limits.cpu", "requests.cpu":
		if !slices.Contains(cpuDivisors, d) {
			errs = append(errs, invalid(path+".divisor", r.Resource, cpuDivisorMessage))
		}
	case "limits.memory", "requests.memory":
		if !slices.Contains(byteDivisors, d) {
			errs = append(errs, invalid(path+".divisor", r.Resource, memDivisorMessage))
		}
	case "limits.ephemeral-storage", "requests.ephemeral-storage":
		if !slices.Contains(byteDivisors, d) {
			errs = append(errs, invalid(path+".divisor", r.Resource, ephDivisorMessage))
		}
	}
	return errs
}

// validateLocalPath checks a path inside a volume: relative, without "..".
func validateLocalPath(path, target string) []string {
	var errs []string
	if filepath.IsAbs(target) {
		errs = append(errs, invalid(path, target, "must be a relative path"))
	}
	if slices.Contains(strings.Split(filepath.ToSlash(target), "/"), "..") {
		errs = append(errs, invalid(path, target, "must not contain '..'"))
	}
	if strings.HasPrefix(target, "..") {
		errs = append(errs, invalid(path, target, "must not start with '..'"))
	}
	return errs
}

func validateMode(path string, mode *int32) []string {
	if mode != nil && (*mode < 0 || *mode > 0o777) {
		return []string{invalid(path, *mode, fileModeMessage)}
	}
	return nil
}

func validateKeyToPath(path string, kp corev1.KeyToPath) []string {
	var errs []string
	if kp.Key == "" {
		errs = append(errs, path+".key: Required value")
	}
	if kp.Path == "" {
		errs = append(errs, path+".path: Required value")
	}
	errs = append(errs, validateLocalPath(path+".path", kp.Path)...)
	return append(errs, validateMode(path+".mode", kp.Mode)...)
}

func validateItems(path string, items []corev1.KeyToPath) []string {
	var errs []string
	for i, kp := range items {
		errs = append(errs, validateKeyToPath(fmt.Sprintf("%s.items[%d]", path, i), kp)...)
	}
	return errs
}

// validateDownwardAPIFile checks one file of a downward API volume or
// projection; like Kubernetes, the errors name the volume, not the item.
func validateDownwardAPIFile(path string, f corev1.DownwardAPIVolumeFile) []string {
	var errs []string
	if f.Path == "" {
		errs = append(errs, path+".path: Required value")
	}
	errs = append(errs, validateLocalPath(path+".path", f.Path)...)
	switch {
	case f.FieldRef != nil:
		errs = append(errs, validateFieldRef(path+".fieldRef", f.FieldRef, volumeFieldPaths)...)
		if f.ResourceFieldRef != nil {
			errs = append(errs, invalid(path, "resource", "fieldRef and resourceFieldRef can not be specified simultaneously"))
		}
	case f.ResourceFieldRef != nil:
		errs = append(errs, validateResourceFieldRef(path+".resourceFieldRef", f.ResourceFieldRef, true)...)
	default:
		errs = append(errs, path+": Required value: one of fieldRef and resourceFieldRef is required")
	}
	return append(errs, validateMode(path+".mode", f.Mode)...)
}

// validateProjectedSources checks the sources of a projected volume; a path
// may be used by one of them only.
func validateProjectedSources(path string, p *corev1.ProjectedVolumeSource) []string {
	errs := validateMode(path+".defaultMode", p.DefaultMode)
	paths := map[string]bool{}
	claim := func(target, name string) {
		if target == "" {
			return
		}
		if paths[target] {
			errs = append(errs, invalid(path, name, conflictingPaths))
		}
		paths[target] = true
	}
	for i, s := range p.Sources {
		sp := fmt.Sprintf("%s.sources[%d]", path, i)
		kinds := 0
		if s.Secret != nil {
			kinds++
			if s.Secret.Name == "" {
				errs = append(errs, sp+".secret.name: Required value")
			}
			errs = append(errs, validateItems(sp+".secret", s.Secret.Items)...)
			for _, kp := range s.Secret.Items {
				claim(kp.Path, s.Secret.Name)
			}
		}
		if s.ConfigMap != nil {
			kinds++
			if s.ConfigMap.Name == "" {
				errs = append(errs, sp+".configMap.name: Required value")
			}
			errs = append(errs, validateItems(sp+".configMap", s.ConfigMap.Items)...)
			for _, kp := range s.ConfigMap.Items {
				claim(kp.Path, s.ConfigMap.Name)
			}
		}
		if s.DownwardAPI != nil {
			kinds++
			for _, f := range s.DownwardAPI.Items {
				errs = append(errs, validateDownwardAPIFile(sp+".downwardAPI", f)...)
				claim(f.Path, f.Path)
			}
		}
		if kinds > 1 {
			errs = append(errs, sp+": Forbidden: may not specify more than 1 volume type per source")
		}
	}
	return errs
}

// validateVolumeSource checks that a volume has exactly one source and
// checks that source.
func validateVolumeSource(p string, v corev1.VolumeSource) []string {
	var errs []string
	kinds := 0
	add := func(name string, check func() []string) {
		if kinds > 0 {
			errs = append(errs, p+"."+name+": Forbidden: may not specify more than 1 volume type")
			return
		}
		kinds++
		errs = append(errs, check()...)
	}
	if e := v.EmptyDir; e != nil {
		add("emptyDir", func() []string {
			if e.Medium != "" && e.Medium != corev1.StorageMediumMemory {
				return []string{fmt.Sprintf(`%s.emptyDir.medium: Unsupported value: %q: supported values: "", "Memory"`, p, e.Medium)}
			}
			return nil
		})
	}
	if h := v.HostPath; h != nil {
		add("hostPath", func() []string { return validateHostPath(p+".hostPath", h) })
	}
	if c := v.ConfigMap; c != nil {
		add("configMap", func() []string {
			var errs []string
			if c.Name == "" {
				errs = append(errs, p+".configMap.name: Required value")
			}
			errs = append(errs, validateMode(p+".configMap.defaultMode", c.DefaultMode)...)
			return append(errs, validateItems(p+".configMap", c.Items)...)
		})
	}
	if s := v.Secret; s != nil {
		add("secret", func() []string {
			var errs []string
			if s.SecretName == "" {
				errs = append(errs, p+".secret.secretName: Required value")
			}
			errs = append(errs, validateMode(p+".secret.defaultMode", s.DefaultMode)...)
			return append(errs, validateItems(p+".secret", s.Items)...)
		})
	}
	if d := v.DownwardAPI; d != nil {
		add("downwardAPI", func() []string {
			errs := validateMode(p+".downwardAPI.defaultMode", d.DefaultMode)
			for _, f := range d.Items {
				errs = append(errs, validateDownwardAPIFile(p+".downwardAPI", f)...)
			}
			return errs
		})
	}
	if pr := v.Projected; pr != nil {
		add("projected", func() []string { return validateProjectedSources(p+".projected", pr) })
	}
	if c := v.PersistentVolumeClaim; c != nil {
		add("persistentVolumeClaim", func() []string {
			if c.ClaimName == "" {
				return []string{p + ".persistentVolumeClaim.claimName: Required value"}
			}
			return nil
		})
	}
	if kinds == 0 {
		errs = append(errs, p+": Required value: must specify a volume type")
	}
	return errs
}
