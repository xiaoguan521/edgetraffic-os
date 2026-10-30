package ipsets

import (
	"net/netip"
	"reflect"
	"strings"
	"testing"
)

const sample = `2|apnic|20261030|5|19830613|20261029|+1000
apnic|*|ipv4|*|3|summary
apnic|CN|ipv4|1.0.1.0|256|20110414|allocated
apnic|CN|ipv4|1.0.2.0|768|20110414|allocated
apnic|JP|ipv4|1.0.16.0|4096|20110412|allocated
apnic|CN|ipv6|2001:250::|35|20000426|allocated
apnic|CN|ipv4|1.1.1.0|256|20110414|reserved
`

func TestParseRIR(t *testing.T) {
	got, err := ParseRIR(strings.NewReader(sample), "cn", 100)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"1.0.1.0/24", "1.0.2.0/23", "1.0.4.0/24", "2001:250::/35"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
	if _, err := ParseRIR(strings.NewReader(sample), "CHN", 100); err == nil {
		t.Fatal("3-letter code accepted")
	}
	if _, err := ParseRIR(strings.NewReader(sample), "US", 100); err == nil {
		t.Fatal("empty result accepted")
	}
	if _, err := ParseRIR(strings.NewReader(sample), "CN", 2); err == nil {
		t.Fatal("limit not enforced")
	}
}

func TestRangeToCIDRs(t *testing.T) {
	cases := map[string][]string{
		"10.0.0.0/256":   {"10.0.0.0/24"},
		"10.0.0.128/384": {"10.0.0.128/25", "10.0.1.0/24"},
		"10.0.0.1/3":     {"10.0.0.1/32", "10.0.0.2/31"},
	}
	for in, want := range cases {
		p := strings.SplitN(in, "/", 2)
		var n uint32
		for _, c := range p[1] {
			n = n*10 + uint32(c-'0')
		}
		a, _ := parse(p[0])
		got, err := rangeToCIDRs(a, n)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("%s: %v %v", in, got, err)
		}
	}
}

func parse(s string) (netip.Addr, error) { return netip.ParseAddr(s) }
