package orderer

import (
	"fmt"
	"os"
	"sync"

	"github.com/hyperledger-labs/mirbft/manager"
)

// MultiPaxosMulticastOrderer estende MultiPaxosOrderer com suporte a grupos
// Usa modelo de logs paralelos + barreira global (já implementado no base)
type MultiPaxosMulticastOrderer struct {
	*MultiPaxosOrderer
	groupsFile string
	mu         sync.RWMutex
}

func (o *MultiPaxosMulticastOrderer) Init(mngr manager.Manager) {
	if o.MultiPaxosOrderer == nil {
		o.MultiPaxosOrderer = &MultiPaxosOrderer{}
	}
	
	o.MultiPaxosOrderer.Init(mngr)
	
	// Carrega configuração de grupos do arquivo YAML (se existir)
	if o.groupsFile != "" {
		err := o.MultiPaxosOrderer.am.LoadGroupsFromYAML(o.groupsFile)
		if err != nil {
			if os.IsNotExist(err) {
				fmt.Printf("[MULTICAST] groups.yml não encontrado (%s), rodando em broadcast mode\n", o.groupsFile)
			} else {
				fmt.Printf("[MULTICAST][ERRO] Falha ao carregar groups.yml: %v\n", err)
			}
		} else {
			fmt.Printf("[MULTICAST] groups.yml carregado: %s\n", o.groupsFile)
		}
	}
}



func (o *MultiPaxosMulticastOrderer) LoadGroupsFromYAML(filename string) error {
	if o.MultiPaxosOrderer == nil || o.MultiPaxosOrderer.am == nil {
		o.groupsFile = filename
		return nil
	}
	return o.MultiPaxosOrderer.am.LoadGroupsFromYAML(filename)
}