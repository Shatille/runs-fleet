export interface Pool {
  pool_name: string;
  instance_type?: string;
  desired_running: number;
  desired_stopped: number;
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
  arch: string;
  cpu_min: number;
  cpu_max: number;
  ram_min: number;
  ram_max: number;
  families: string[];
}
