package apiserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strconv"
	"time"

	metav1 "mini-k8s/pkg/apis/meta/v1"
	"mini-k8s/pkg/scheme"
	"mini-k8s/pkg/storage/etcd3"
)

func (s *Server) key(res scheme.ResourceInfo, namespace, name string) string {
	if res.Namespaced {
		return s.cfg.Prefix + "/" + res.Resource + "/" + namespace + "/" + name
	}
	return s.cfg.Prefix + "/" + res.Resource + "/" + name
}

// listPrefix returns the key prefix for a namespace, or for all namespaces
// when namespace is empty.
func (s *Server) listPrefix(res scheme.ResourceInfo, namespace string) string {
	p := s.cfg.Prefix + "/" + res.Resource + "/"
	if res.Namespaced && namespace != "" {
		p += namespace + "/"
	}
	return p
}

func (s *Server) decode(res scheme.ResourceInfo, kv etcd3.KV) (metav1.Object, error) {
	obj := res.New()
	if err := json.Unmarshal(kv.Value, obj); err != nil {
		return nil, err
	}
	res.SetTypeMeta(obj)
	obj.GetObjectMeta().ResourceVersion = strconv.FormatInt(kv.ModRevision, 10)
	return obj, nil
}

// encode serialises obj without its resourceVersion, which etcd tracks.
func encode(obj metav1.Object) ([]byte, error) {
	m := obj.GetObjectMeta()
	rv := m.ResourceVersion
	m.ResourceVersion = ""
	defer func() { m.ResourceVersion = rv }()
	return json.Marshal(obj)
}

func (s *Server) get(ctx context.Context, res scheme.ResourceInfo, namespace, name string) (metav1.Object, error) {
	kv, err := s.cfg.Store.Get(ctx, s.key(res, namespace, name))
	if errors.Is(err, etcd3.ErrNotFound) {
		return nil, errNotFound(res, name)
	}
	if err != nil {
		return nil, err
	}
	return s.decode(res, kv)
}

func (s *Server) list(ctx context.Context, res scheme.ResourceInfo, namespace string) ([]metav1.Object, int64, error) {
	kvs, rev, err := s.cfg.Store.List(ctx, s.listPrefix(res, namespace))
	if err != nil {
		return nil, 0, err
	}
	objs := make([]metav1.Object, 0, len(kvs))
	for _, kv := range kvs {
		obj, err := s.decode(res, kv)
		if err != nil {
			return nil, 0, err
		}
		objs = append(objs, obj)
	}
	return objs, rev, nil
}

func (s *Server) leaseFor(ctx context.Context, res scheme.ResourceInfo) (int64, error) {
	if res.Resource != scheme.Events.Resource {
		return 0, nil
	}
	s.leaseMu.Lock()
	defer s.leaseMu.Unlock()
	// Events share a lease per minute so they expire about an hour after
	// their last update without creating one lease per write.
	if s.eventLease != 0 && time.Since(s.eventLeaseAt) < time.Minute {
		return s.eventLease, nil
	}
	id, err := s.cfg.Store.GrantLease(ctx, s.cfg.EventTTL+time.Minute)
	if err != nil {
		return 0, err
	}
	s.eventLease, s.eventLeaseAt = id, time.Now()
	return id, nil
}

func (s *Server) create(ctx context.Context, res scheme.ResourceInfo, obj metav1.Object) (metav1.Object, error) {
	data, err := encode(obj)
	if err != nil {
		return nil, err
	}
	lease, err := s.leaseFor(ctx, res)
	if err != nil {
		return nil, err
	}
	m := obj.GetObjectMeta()
	rev, err := s.cfg.Store.Create(ctx, s.key(res, m.Namespace, m.Name), data, lease)
	if errors.Is(err, etcd3.ErrAlreadyExists) {
		return nil, errAlreadyExists(res, m.Name)
	}
	if err != nil {
		return nil, err
	}
	m.ResourceVersion = strconv.FormatInt(rev, 10)
	return obj, nil
}

// update writes obj if the stored revision still equals obj's resourceVersion.
func (s *Server) update(ctx context.Context, res scheme.ResourceInfo, obj metav1.Object) (metav1.Object, error) {
	m := obj.GetObjectMeta()
	expected, err := strconv.ParseInt(m.ResourceVersion, 10, 64)
	if err != nil {
		return nil, errBadRequest("invalid resourceVersion %q", m.ResourceVersion)
	}
	data, err := encode(obj)
	if err != nil {
		return nil, err
	}
	lease, err := s.leaseFor(ctx, res)
	if err != nil {
		return nil, err
	}
	rev, err := s.cfg.Store.Update(ctx, s.key(res, m.Namespace, m.Name), data, expected, lease)
	switch {
	case errors.Is(err, etcd3.ErrNotFound):
		return nil, errNotFound(res, m.Name)
	case errors.Is(err, etcd3.ErrConflict):
		return nil, errConflict(res, m.Name)
	case err != nil:
		return nil, err
	}
	m.ResourceVersion = strconv.FormatInt(rev, 10)
	return obj, nil
}

func (s *Server) remove(ctx context.Context, res scheme.ResourceInfo, obj metav1.Object) error {
	m := obj.GetObjectMeta()
	expected, err := strconv.ParseInt(m.ResourceVersion, 10, 64)
	if err != nil {
		return errBadRequest("invalid resourceVersion %q", m.ResourceVersion)
	}
	_, err = s.cfg.Store.Delete(ctx, s.key(res, m.Namespace, m.Name), expected)
	switch {
	case errors.Is(err, etcd3.ErrNotFound):
		return errNotFound(res, m.Name)
	case errors.Is(err, etcd3.ErrConflict):
		return errConflict(res, m.Name)
	}
	return err
}

// decodeStrict parses a request body, rejecting unknown fields.
func decodeStrict(data []byte, obj any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(obj); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return errors.New("trailing data after JSON object")
	}
	return nil
}

// field returns obj's exported struct field by name, if it exists.
func field(obj metav1.Object, name string) (reflect.Value, bool) {
	v := reflect.ValueOf(obj).Elem().FieldByName(name)
	return v, v.IsValid()
}

// copyField sets dst.name = src.name for a field both objects share.
func copyField(dst, src metav1.Object, name string) {
	d, ok := field(dst, name)
	if !ok {
		return
	}
	sv, _ := field(src, name)
	d.Set(sv)
}

// resetField zeroes obj.name.
func resetField(obj metav1.Object, name string) {
	if v, ok := field(obj, name); ok {
		v.Set(reflect.Zero(v.Type()))
	}
}

// specChanged reports whether the objects' Spec fields differ.
func specChanged(a, b metav1.Object) bool {
	av, ok := field(a, "Spec")
	if !ok {
		return false
	}
	bv, _ := field(b, "Spec")
	aj, _ := json.Marshal(av.Interface())
	bj, _ := json.Marshal(bv.Interface())
	return !bytes.Equal(aj, bj)
}

// sameStoredForm reports whether two objects would serialise identically.
func sameStoredForm(a, b metav1.Object) bool {
	aj, err1 := encode(a)
	bj, err2 := encode(b)
	return err1 == nil && err2 == nil && bytes.Equal(aj, bj)
}
