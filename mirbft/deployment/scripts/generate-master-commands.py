Bruno@node-0:/tmp/ISS_com_Multipaxos/mirbft/deployment$ ./deploy.sh remote scripts/instance-info new scripts/experiment-configuration/generate-config.sh
Using experiment data directory: deployment-data/remote-0000
Generated 4 experiments.
[initialize-deployment] depl_type = remote
[initialize-deployment] instance_info_file = scripts/instance-info
[initialize-deployment] new_experiment      = false
[initialize-deployment] exp_data_dir        = deployment-data/remote-0000
[initialize-deployment] config_gen_script   = <none>
[initialize-deployment] deployment_file     = deployment-data/remote-0000/deployment.dpl
[initialize-deployment] exp_id_offset       = 4

==================================================
[BUILD] Preflight: garantindo binários locais
==================================================
[initialize-deployment] repo_dir      = /tmp/ISS_com_Multipaxos/mirbft
[initialize-deployment] local_bin_dir = /users/Bruno/go/bin
[initialize-deployment] go version    = go version go1.23.1 linux/amd64

[initialize-deployment] OK: todos os binários necessários já existem.
[INFO  ][2025-12-12 17:22:50] Using instance info file: /tmp/ISS_com_Multipaxos/mirbft/deployment/scripts/instance-info
[INFO  ][2025-12-12 17:22:50] Master IP address      : 172.20.6.3

[INFO  ][2025-12-12 17:22:50] Gerando master commands via generate-master-commands.py
[INFO  ][2025-12-12 17:22:50]   deployment_file (.dpl) = deployment-data/remote-0000/deployment.dpl
[INFO  ][2025-12-12 17:22:50]   template out           = deployment-data/remote-0000/master-commands-template.cmd
[generate-master-commands] 4 experimentos detectados. Último exp = 0003
Traceback (most recent call last):
  File "/tmp/ISS_com_Multipaxos/mirbft/deployment/scripts/generate-master-commands.py", line 120, in <module>
    cfg = exp["config"]
KeyError: 'config'

