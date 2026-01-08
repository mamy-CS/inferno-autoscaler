package collector

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestCollectorV2(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Collector V2 Suite")
}
