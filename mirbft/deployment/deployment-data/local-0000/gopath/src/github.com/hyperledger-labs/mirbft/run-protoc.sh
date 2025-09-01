#!/bin/bash
set -e

# Diretório base do projeto
BASE_DIR="$( cd "$( dirname "${BASH_SOURCE[0]}" )" && pwd )"

# Diretório onde estão os .proto
PROTO_DIR="$BASE_DIR/protobufs"

# Diretório de saída do código Go gerado
OUT_DIR="$PROTO_DIR"

echo "Gerando código Go a partir dos arquivos .proto..."

for proto_file in "$PROTO_DIR"/*.proto; do
    echo "Processando $proto_file..."
    protoc \
        -I="$PROTO_DIR" \
        --go_out="$OUT_DIR" \
        --go-grpc_out="$OUT_DIR" \
        "$proto_file"
done

echo "Geração concluída!"

