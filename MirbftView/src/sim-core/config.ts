import { SimulatorConfig } from './types';

export const DEFAULT_CONFIG: SimulatorConfig = {
  simulation: {
    protocol: 'MultiPaxosMulticast',
    mode: 'step_by_step',
    speed: 1.0,
    random_seed: 1,
  },
  network: {
    num_nodes: 5,
    clients: 2,
    latency_ms: 20,
    jitter_ms: 5,
    packet_loss: 0.0,
  },
  groups: {
    sequencer_group: 0,
    data_groups: {
      1: [0, 1, 2],
      2: [2, 3, 4],
      3: [0, 4, 1],
      4: [1, 3, 4],
    },
  },
  orderer: {
    batch_size: 40,
    batch_timeout_ms: 50,
    num_buckets: 16,
    segment_length: 64,
    epoch_length: 64,
    watermark_window_size: 64,
    checkpoint_interval: 64,
    leader_policy: 'round_robin',
  },
  client: {
    request_rate: 2000,
    requests_per_client: 1000,
    payload_size: 250,
    workload: [
      { weight: 80, pattern: 'GET K{seqNr:08d}' },
      { weight: 20, pattern: 'TX K{seqNr:08d},K{seqNr+1000:08d}' },
    ],
  },
  visual: {
    show_packets: true,
    show_buckets: true,
    show_gsn: true,
    show_meta_stream: true,
    show_checkpoints: true,
    show_watermarks: true,
    show_event_log: true,
    show_timeline: true,
  },
};
