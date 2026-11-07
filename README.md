# ISS com MultiPaxos: difusão atômica seletiva

Extensão do arcabouço ISS (Insanely Scalable State-Machine Replication), sobre a implementação [MirBFT](https://github.com/hyperledger-labs/mirbft) (branch `research-iss`), com uma interface de difusão atômica seletiva no estilo `multicast(g, m)` / `deliver(m)`.

O repositório traz dois ordenadores novos, além dos originais do ISS (PBFT, HotStuff e Raft):

- **MultiPaxos:** consenso MultiPaxos com difusão para todas as réplicas (broadcast). É a linha de base dos experimentos.
- **MultiPaxosMulticast:** cada grupo de réplicas ordena as próprias mensagens com MultiPaxos. As operações que tocam vários grupos (cross-group) recebem um número global (GSN) de um sequenciador tolerante a falhas e são entregues na mesma ordem em todos os grupos.

## Estrutura

| Pasta | Conteúdo |
|---|---|
| `mirbft/` | Código Go: ordenadores (`orderer/`), cliente, réplica e ferramentas (`cmd/`), configuração dos grupos (`config/groups.yml`) |
| `mirbft/deployment/` | Scripts de implantação e geradores de configuração dos experimentos |
| `MirbftView/` | Simulador visual (Python/Qt) do MultiPaxosMulticast, para fins didáticos |

## Compilação

Requer Go 1.21 ou mais recente e Linux.

```sh
cd mirbft
go build ./...
```

Os resultados do artigo usaram binários compilados com **Go 1.23.4**. A partir do Go 1.24, a assinatura ECDSA da biblioteca padrão ficou mais lenta em processadores sem AVX2, como o Xeon E5530 das máquinas d710 do Emulab. Como o cliente assina cada requisição, a vazão medida nessas máquinas cai ~15%.

## Execução dos experimentos

Os passos completos estão na seção "Reprodutibilidade" do artigo. Em resumo:

1. São necessárias sete máquinas Linux na mesma rede: um master, cinco réplicas e um cliente. O master precisa de acesso SSH sem senha a todos os nós.
2. Descreva as máquinas em `mirbft/deployment/scripts/instance-info`, uma por linha: nome, IP público, IP privado, tipo e papel.
3. Escolha os parâmetros em `mirbft/deployment/scripts/experiment-configuration/generate-local-config.sh`.
4. Execute, a partir de `mirbft/deployment`:

   ```sh
   ./deploy.sh remote scripts/instance-info new \
       scripts/experiment-configuration/generate-local-config.sh
   ```

5. Os resultados ficam em `mirbft/deployment/deployment-data/`, com um `result-summary.csv` por execução.

A chave SSH usada pelos scripts de nuvem (`mirbft/deployment/ibmcloud-ssh-key`) não faz parte do repositório: `scripts/setup/init.sh` gera uma chave nova quando ela não existe. As chaves em `tls-data/` são de teste e servem apenas para a comunicação TLS entre os nós dos experimentos.

## Licença

Apache 2.0, a mesma do MirBFT, do qual este código deriva. Veja [LICENSE](LICENSE).
