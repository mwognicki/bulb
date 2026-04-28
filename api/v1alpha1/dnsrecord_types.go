package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// DNSRecord represents the desired DNS configuration for a bulb-managed
// Service. Phase 3 (dry-run): the controller creates these for
// informational purposes only — no agent consumes them yet. Phase 5
// will add status fields and a dns-agent that publishes them.
type DNSRecord struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec DNSRecordSpec `json:"spec,omitempty"`
}

type DNSRecordSpec struct {
	FQDN    string   `json:"fqdn"`
	Type    string   `json:"type,omitempty"`
	TTL     int32    `json:"ttl,omitempty"`
	Targets []string `json:"targets,omitempty"`
}

// DNSRecordList is a list of DNSRecord objects.
type DNSRecordList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DNSRecord `json:"items"`
}

func init() {
	SchemeBuilder.Register(&DNSRecord{}, &DNSRecordList{})
}

func (in *DNSRecord) DeepCopyInto(out *DNSRecord) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Spec = in.Spec.DeepCopy()
}

func (in *DNSRecord) DeepCopy() *DNSRecord {
	if in == nil {
		return nil
	}
	out := new(DNSRecord)
	in.DeepCopyInto(out)
	return out
}

func (in *DNSRecord) DeepCopyObject() runtime.Object {
	return in.DeepCopy()
}

func (in DNSRecordSpec) DeepCopy() DNSRecordSpec {
	out := in
	if in.Targets != nil {
		out.Targets = append([]string(nil), in.Targets...)
	}
	return out
}

func (in *DNSRecordList) DeepCopyInto(out *DNSRecordList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]DNSRecord, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

func (in *DNSRecordList) DeepCopy() *DNSRecordList {
	if in == nil {
		return nil
	}
	out := new(DNSRecordList)
	in.DeepCopyInto(out)
	return out
}

func (in *DNSRecordList) DeepCopyObject() runtime.Object {
	return in.DeepCopy()
}
