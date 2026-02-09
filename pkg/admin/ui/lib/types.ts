export interface Pool {
  pool_name: string;
  instance_type?: string;
  desired_running: number;
  desired_stopped: number;
  current_running: number;
  current_stopped: number;
  busy_instances: number;
  idle_timeout_minutes?: number;
  ephemeral: boolean;
  environment?: string;
  region?: string;
  arch?: string;
  cpu_min?: number;
  cpu_max?: number;
  ram_min?: number;
  ram_max?: number;
  families?: string[];
  schedules?: Schedule[];
}

export interface Schedule {
  name: string;
  start_hour: number;
  end_hour: number;
  days_of_week?: number[];
  desired_running: number;
  desired_stopped: number;
}

export interface PoolFormData {
  pool_name: string;
  instance_type: string;
  desired_running: number;
  desired_stopped: number;
  idle_timeout_minutes: number;
  environment: string;
  region: string;
  arch: string;
  cpu_min: number;
  cpu_max: number;
  ram_min: number;
  ram_max: number;
  families: string[];
  schedules: Schedule[];
}

export interface Job {
  job_id: number;
  run_id?: number;
  repo?: string;
  instance_id?: string;
  instance_type?: string;
  pool?: string;
  spot: boolean;
  warm_pool_hit: boolean;
  retry_count: number;
  status: string;
  exit_code?: number;
  duration_seconds?: number;
  created_at?: string;
  started_at?: string;
  completed_at?: string;
}

export interface JobStats {
  total: number;
  completed: number;
  failed: number;
  running: number;
  requeued: number;
  warm_pool_hit: number;
  hit_rate: number;
}

export interface Instance {
  instance_id: string;
  instance_type: string;
  pool: string;
  state: string;
  launch_time: string;
  private_ip?: string;
  spot: boolean;
  busy: boolean;
}

export interface QueueStatus {
  name: string;
  url: string;
  messages_visible: number;
  messages_in_flight: number;
  messages_delayed: number;
  dlq_messages: number;
}

export interface CircuitState {
  instance_type: string;
  state: string;
  failure_count: number;
  last_failure?: string;
  reset_at?: string;
}
