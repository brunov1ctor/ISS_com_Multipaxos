/* Cenário do orientador: p1 e p2 estão em G1 e G2.
 *
 * A ordem total é do SISTEMA, escolhida no deploy:
 *   ./deploy.sh ... --order local    (padrão)
 *   ./deploy.sh ... --order global
 *
 * local : PUT(x) antes de GET(x) e PUT(y) antes de GET(y) em todo nó, mas a intercalação entre
 *         G1 e G2 pode mudar de um nó para outro.
 * global: todo nó em G1∩G2 entrega as mensagens na mesma sequência (compare as linhas
 *         [GDELIVER] nos peer.log dos nós).
 *
 * Compilar (Linux, com libiss_multicast.so e iss_multicast.h nesta pasta):
 *   gcc main.c -o exemplo -L. -liss_multicast -Wl,-rpath,'$ORIGIN'
 * Uso:
 *   ./exemplo <config.yml> <discovery host:port>            -- só o cenário do orientador
 *   ./exemplo <config.yml> <discovery host:port> <N>        -- + N multicasts em paralelo
 *                                                              alternando G1/G2 (carga)
 */
#include <pthread.h>
#include <stdio.h>
#include <stdlib.h>
#include "iss_multicast.h"

static void check(int rc, const char *what) {
    if (rc < 0) {
        char *e = iss_last_error();
        fprintf(stderr, "%s falhou: %s\n", what, e);
        iss_free(e);
    }
}

#define THREADS 8
static int total;

static void *worker(void *arg) {
    long id = (long)arg;
    char msg[64];
    for (int i = id; i < total; i += THREADS) {
        snprintf(msg, sizeof msg, "MSG-%d-G%d", i, 1 + i % 2);
        check(iss_multicast(1 + i % 2, msg), msg);
    }
    return NULL;
}

int main(int argc, char **argv) {
    if (argc < 3) {
        fprintf(stderr, "uso: %s <config.yml> <discovery host:port> [N]\n", argv[0]);
        return 1;
    }
    if (iss_connect(argv[1], argv[2]) < 0) { check(-1, "iss_connect"); return 1; }

    check(iss_put(1, "x", "10"), "PUT(x,10)");
    check(iss_put(2, "y", "10"), "PUT(y,10)");

    char *v = NULL;
    if (iss_get(1, "x", &v) == 1) { printf("GET(x) = %s\n", v); iss_free(v); }
    if (iss_get(2, "y", &v) == 1) { printf("GET(y) = %s\n", v); iss_free(v); }

    if (argc > 3) {
        total = atoi(argv[3]);
        pthread_t t[THREADS];
        for (long i = 0; i < THREADS; i++) pthread_create(&t[i], NULL, worker, (void *)i);
        for (int i = 0; i < THREADS; i++) pthread_join(t[i], NULL);
        printf("%d multicasts enviados (G1/G2 alternados, %d threads)\n", total, THREADS);
    }

    iss_close();
    return 0;
}
