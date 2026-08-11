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
  override_linger_minutes?: number | null;
  override_max_hot?: number | null;
  auto_tune?: AutoTuneRec;
}

export interface AutoTuneRec {
  recommended_linger_minutes?: number;
  recommended_max_hot?: number;
  window_days?: number;
  job_count?: number;
  burst_count?: number;
  p90_intra_burst_gap_seconds?: number;
  peak_concurrency?: number;
  reason?: string;
  tuned_at?: string;
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
  override_linger_minutes: number | null;
  override_max_hot: number | null;
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
  // How long an unfinished job has been open. duration_seconds is written only
  // at completion, so without this a job hung for six hours reports nothing.
  elapsed_seconds?: number;
  stalled?: boolean;
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
  stalled: number;
  warm_pool_hit: number;
  hit_rate: number;
}

// What GitHub says about a job our own record still calls open. 'hung' is the
// only definitive verdict: GitHub still has it queued, so nothing is running it.
export type HungClassification = 'hung' | 'running' | 'completed_upstream' | 'unknown';

export interface HungJob extends Job {
  github_status?: string;
  github_conclusion?: string;
  runner_name?: string;
  classification: HungClassification;
  detail?: string;
}

export interface HungJobsResponse {
  jobs: HungJob[];
  candidates: number;
  checked: number;
  truncated: boolean;
  github_available: boolean;
  stale_minutes: number;
}

export interface GitHubJobStatus {
  job_id: number;
  repo: string;
  status: string;
  conclusion?: string;
  runner_name?: string;
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
  image_id?: string;
  architecture?: string;
  // Not running what this instance's own architecture would launch today.
  // Always false while the reference AMI is unknown.
  ami_stale?: boolean;
}

export interface CurrentAMI {
  architecture: string;
  image_id: string;
  launch_template: string;
  version: number;
  version_created?: string;
}

export interface ReplaceStaleResult {
  terminated?: string[];
  busy?: string[];
  skipped?: string[];
  stale: number;
  dry_run: boolean;
  message: string;
}

export interface CurrentAMIsResponse {
  amis: CurrentAMI[];
  unresolved?: string[];
}

export interface InstanceDetail extends Instance {
  availability_zone?: string;
  image_id?: string;
  subnet_id?: string;
  architecture?: string;
  state_reason?: string;
  tags?: Record<string, string>;
}

export interface ActiveJobRef {
  job_id: number;
  run_id: number;
  repo: string;
}

export interface RequeueJobResult {
  job_id: number;
  outcome: string;
  instance_id?: string;
  instance_terminated: boolean;
  retry_count: number;
  status?: string;
  message: string;
  details?: string;
}

export interface ReconcileJobResult {
  job_id: number;
  outcome: string;
  orphaned: boolean;
  instance_id?: string;
  status?: string;
  message: string;
  details?: string;
}

export interface OrphanedInstancesResult {
  instance_ids?: string[];
  candidates: number;
  terminated: number;
  dry_run: boolean;
  message: string;
}

export interface TerminateInstanceResult {
  instance_id: string;
  pool?: string;
  state?: string;
  forced: boolean;
  active_job?: ActiveJobRef;
  message: string;
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
  total_minutes: number;
  cost_per_minute: number;
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
  cost_per_minute: number;
  baseline_cost: number;
  baseline_cost_per_minute: number;
}

export interface FamilyBreakdown {
  family: string;
  job_count: number;
  total_hours: number;
  total_cost: number;
  cost_per_minute: number;
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
  total_minutes: number;
  cost_per_minute: number;
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
  total_minutes: number;
  cost_per_minute: number;
  spot_percent: number;
}

export interface CostByRepository {
  period_start: string;
  period_end: string;
  repositories: CostRepositoryEntry[];
}

export interface CostRepositoryEntry {
  repository: string;
  job_count: number;
  total_cost: number;
  spot_cost: number;
  on_demand_cost: number;
  avg_cost_per_job: number;
  total_minutes: number;
  cost_per_minute: number;
  spot_percent: number;
}

export interface MetricsSummary {
  jobs_24h: {
    total: number;
    completed: number;
    failed: number;
    in_progress: number;
  };
  warm_pool_hit_rate: number;
  avg_startup_time_seconds: number;
  spot_interruption_rate: number;
  spot_interruption_rate_estimated: boolean;
  cost_mtd_usd?: number;
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
