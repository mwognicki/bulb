package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

const (
	Group   = "bulb.toturi.tech"
	Version = "v1alpha1"
)

var (
	GroupVersion  = schema.GroupVersion{Group: Group, Version: Version}
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}
	AddToScheme   = SchemeBuilder.AddToScheme
)

// LBPort declares a port/protocol that bulb-managed nodes should expose.
type LBPort struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   LBPortSpec   `json:"spec,omitempty"`
	Status LBPortStatus `json:"status,omitempty"`
}

type LBPortSpec struct {
	Port            int32           `json:"port"`
	Protocol        corev1.Protocol `json:"protocol"`
	Nodes           []string        `json:"nodes,omitempty"`
	Owner           string          `json:"owner,omitempty"`
	AllowPrivileged bool            `json:"allowPrivileged,omitempty"`
}

type LBPortStatus struct {
	AppliedNodes []string `json:"appliedNodes,omitempty"`
}

// LBPortList is a list of LBPort objects.
type LBPortList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []LBPort `json:"items"`
}

func init() {
	SchemeBuilder.Register(&LBPort{}, &LBPortList{})
}

func (in *LBPort) DeepCopyInto(out *LBPort) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Spec = in.Spec.DeepCopy()
	out.Status = in.Status.DeepCopy()
}

func (in *LBPort) DeepCopy() *LBPort {
	if in == nil {
		return nil
	}
	out := new(LBPort)
	in.DeepCopyInto(out)
	return out
}

func (in *LBPort) DeepCopyObject() runtime.Object {
	return in.DeepCopy()
}

func (in LBPortSpec) DeepCopy() LBPortSpec {
	out := in
	if in.Nodes != nil {
		out.Nodes = append([]string(nil), in.Nodes...)
	}
	return out
}

func (in LBPortStatus) DeepCopy() LBPortStatus {
	out := in
	if in.AppliedNodes != nil {
		out.AppliedNodes = append([]string(nil), in.AppliedNodes...)
	}
	return out
}

func (in *LBPortList) DeepCopyInto(out *LBPortList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]LBPort, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

func (in *LBPortList) DeepCopy() *LBPortList {
	if in == nil {
		return nil
	}
	out := new(LBPortList)
	in.DeepCopyInto(out)
	return out
}

func (in *LBPortList) DeepCopyObject() runtime.Object {
	return in.DeepCopy()
}
