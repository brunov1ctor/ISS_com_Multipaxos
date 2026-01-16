package orderer

import (
	"github.com/hyperledger-labs/mirbft/manager"
)

// MultiPaxosMulticastOrderer é um alias para MultiPaxosOrderer
// Toda lógica CSMR está integrada no MultiPaxosOrderer base
type MultiPaxosMulticastOrderer struct {
	*MultiPaxosOrderer
}

func (o *MultiPaxosMulticastOrderer) Init(mngr manager.Manager) {
	if o.MultiPaxosOrderer == nil {
		o.MultiPaxosOrderer = &MultiPaxosOrderer{}
	}
	o.MultiPaxosOrderer.Init(mngr)
}

func (o *MultiPaxosMulticastOrderer) LoadGroupsFromYAML(filename string) error {
	if o.MultiPaxosOrderer == nil {
		o.MultiPaxosOrderer = &MultiPaxosOrderer{}
	}
	return o.MultiPaxosOrderer.LoadGroupsFromYAML(filename)
}