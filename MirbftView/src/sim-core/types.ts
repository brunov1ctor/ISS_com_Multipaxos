// ============================================================
// Types for the MultiPaxos Multicast Simulator
// Based on: multipaxosmulticastorderer.go, sequencer.go,
//           multipaxosinstance.go, multipaxosgroup.go, bucket.go
// ============================================================

export type PacketType =
  | 'CLIENT_REQUEST'
  | 'GSN_REQUEST'
  | 'GSN_RESPONSE'
  | 'META_STREAM'
  | 'PREPARE'
  | 'PROMISE'
  | 'ACCEPT'
  | 'ACCEPTED'
  | 'COMMIT'
  | 'COMMIT_NOTIFY'
  | 'CHECKPOINT';

export type NodeRole = 'sequencer_leader' | 'group_leader' | 'acceptor' | 'follower' | 'proxy';
export type NodeStatus = 'active' | 'failed' | 'delayed';
export type PaxosPhase = 'INIT' | 'PREPARED' | 'ACCEPT_SENT' | 'COMMITTED';

export interface SimulatorConfig {
  simulation: {
    protocol: string;
    mode: 'step_by_step' | 'continuous';
    speed: number;
    random_seed: number;
  };
  network: {
    num_nodes: number;
    clients: number;
    latency_ms: number;
    jitter_ms: number;
    packet_loss: number;
  };
  groups: {
    sequencer_group: number;
    data_groups: Record<number, number[]>;
  };
  orderer: {
    batch_size: number;
    batch_timeout_ms: number;
    num_buckets: number;
    segment_length: number;
    epoch_length: number;
    watermark_window_size: number;
    checkpoint_interval: number;
    leader_policy: string;
  };
  client: {
    request_rate: number;
    requests_per_client: number;
    payload_size: number;
    workload: { weight: number; pattern: string }[];
  };
  visual: {
    show_packets: boolean;
    show_buckets: boolean;
    show_gsn: boolean;
    show_meta_stream: boolean;
    show_checkpoints: boolean;
    show_watermarks: boolean;
    show_event_log: boolean;
    show_timeline: boolean;
  };
}

export interface Node {
  id: number;
  roles: NodeRole[];
  groups: number[];
  status: NodeStatus;
  delayMs: number;
}

export interface Client {
  id: string;
  nodeId: number; // numeric id for display
  nextSn: number;
  pendingRequests: string[];
}

export interface Group {
  id: number;
  members: number[];
  leader: number;
  isSequencer: boolean;
}

export interface Bucket {
  id: number;
  groupId: number;
  requests: RequestEntry[];
}

export interface RequestEntry {
  id: string;
  clientId: string;
  clientSn: number;
  payload: string;
  touchedGroups: number[];
  gsn: number;
  groupId: number;
  createdAt: number;
}

export interface Batch {
  id: string;
  requests: RequestEntry[];
  groupId: number;
  sn: number;
  digest: string;
}

export interface Packet {
  id: string;
  type: PacketType;
  from: number;
  to: number;
  createdAt: number;
  arriveAt: number;
  groupId?: number;
  sn?: number;
  gsn?: number;
  ballot?: number;
  batch?: Batch;
  requestId?: string;
  metadata?: Record<string, unknown>;
}

export interface MultiPaxosInstance {
  sn: number;
  groupId: number;
  leader: number;
  phase: PaxosPhase;
  ballot: number;
  promiseCount: number;
  acceptedCount: number;
  quorum: number;
  members: number[];
  batch: Batch | null;
  committed: boolean;
}

export interface SequencerState {
  nextGSN: number;
  leader: number;
  members: number[];
  requestsPending: number;
  metadata: Map<number, number[]>; // gsn -> touchedGroups
  publishedMeta: Set<number>;
  groupGSNQueue: Map<number, number[]>; // groupId -> gsn[]
  lastDeliveredGSN: Map<number, number>; // groupId -> lastGSN
  pendingCommits: Map<number, Map<number, PendingCommitEntry>>; // groupId -> gsn -> commit
}

export interface PendingCommitEntry {
  gsn: number;
  groupId: number;
  batch: Batch;
  sn: number;
}

export interface CheckpointState {
  lastLocalCheckpoint: number;
  lastStableCheckpoint: number;
  checkpointInterval: number;
  messagesReceived: Map<number, number>; // nodeId -> count
  quorum: number;
  watermarkLow: number;
  watermarkHigh: number;
}

export interface ExecutionWindow {
  firstSN: number;
  lastSN: number;
  currentSN: number;
  segmentLength: number;
  epochLength: number;
  watermarkLow: number;
  watermarkHigh: number;
  slotsInFlight: number;
  slotsCommitted: number;
}

export interface SimulationEvent {
  id: string;
  time: number;
  type: string;
  from?: string;
  to?: string;
  groupId?: number;
  sn?: number;
  gsn?: number;
  bucketId?: number;
  requestId?: string;
  message?: string;
  metadata?: Record<string, unknown>;
}

export interface TimelineEntry {
  requestId: string;
  events: { label: string; time: number }[];
}

export interface SimulationSnapshot {
  time: number;
  nodes: Node[];
  clients: Client[];
  groups: Group[];
  sequencer: SequencerState;
  instances: MultiPaxosInstance[];
  buckets: Bucket[];
  packets: Packet[];
  checkpoints: CheckpointState;
  executionWindow: ExecutionWindow;
  eventLog: SimulationEvent[];
  timeline: TimelineEntry[];
}
