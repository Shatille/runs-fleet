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
  arch?: string;
  cpu_min?: number;
  cpu_max?: number;
  ram_min?: number;
  ram_max?: number;
  families?: string[];
  schedules?: Schedule[];
  last_reconcile_at?: string;
  last_reconcile_result?: string;
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
  trace_id?: string;
  spot_request_id?: string;
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

export interface InstanceDetail extends Instance {
  availability_zone?: string;
  image_id?: string;
  subnet_id?: string;
  architecture?: string;
  state_reason?: string;
  tags?: Record<string, string>;
}

export interface QueueStatus {
  name: string;
  url: string;
  messages_visible: number;
  messages_in_flight: number;
  messages_delayed: number;
  dlq_messages: number;
  oldest_message_age_seconds?: number;
}

export interface CircuitState {
  instance_type: string;
  state: string;
  failure_count: number;
  last_failure?: string;
  reset_at?: string;
}

export interface CostSummary {
  period_start: string;
  period_end: string;
  total_cost: number;
  spot_cost: number;
  on_demand_cost: number;
  spot_savings: number;
  avg_cost_per_job: number;
  job_count: number;
  spot_job_count: number;
  on_demand_count: number;
  family_breakdown: FamilyBreakdown[];
  runner_minute_cost: number;
  runner_minute_rates: Record<string, number>;
  runner_minute_breakdown: RunnerMinuteEntry[];
}

export interface RunnerMinuteEntry {
  arch: string;
  vcpu: number;
  runner_minutes: number;
  vcpu_minutes: number;
  cost: number;
}

export interface FamilyBreakdown {
  family: string;
  job_count: number;
  total_hours: number;
  total_cost: number;
  spot_percent: number;
}

export interface CostDaily {
  period_start: string;
  period_end: string;
  days: CostDayEntry[];
}

export interface CostDayEntry {
  date: string;
  total_cost: number;
  spot_cost: number;
  on_demand_cost: number;
  job_count: number;
}

export interface CostByPool {
  period_start: string;
  period_end: string;
  pools: CostPoolEntry[];
}

export interface CostPoolEntry {
  pool: string;
  job_count: number;
  total_cost: number;
  spot_cost: number;
  on_demand_cost: number;
  spot_percent: number;
}

export interface AuditEntry {
  id: string;
  user: string;
  action: string;
  target?: string;
  result: string;
  details?: Record<string, unknown>;
  client_ip?: string;
  timestamp: string;
}
