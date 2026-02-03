terraform {
  required_providers {
    datadog = {
      source  = "DataDog/datadog"
      version = "~> 3.0"
    }
  }
}

provider "datadog" {
  api_key = var.datadog_api_key
  app_key = var.datadog_app_key
}

variable "datadog_api_key" {
  type      = string
  sensitive = true
}

variable "datadog_app_key" {
  type      = string
  sensitive = true
}

locals {
  notify  = "@slack-infra-ops-tier1"
  prefix  = "runs_fleet"
  tags    = ["service:runs-fleet", "managed_by:terraform"]
  monitor = "{{#is_alert}}CRITICAL{{/is_alert}}{{#is_warning}}WARNING{{/is_warning}}{{#is_recovery}}RECOVERED{{/is_recovery}}"
}

# -----------------------------------------------------------------------------
# Critical Monitors
# -----------------------------------------------------------------------------

resource "datadog_monitor" "job_failures" {
  name    = "[runs-fleet] Job Failures Spike"
  type    = "metric alert"
  query   = "sum(last_5m):sum:${local.prefix}.job_failure{*} > 10"
  message = "${local.monitor}\n\nJob failure count exceeded threshold.\n\n${local.notify}"

  monitor_thresholds {
    critical = 10
    warning  = 5
  }

  notify_no_data    = false
  renotify_interval = 15
  tags              = local.tags
}

resource "datadog_monitor" "queue_depth" {
  name    = "[runs-fleet] Queue Depth High"
  type    = "metric alert"
  query   = "avg(last_5m):avg:${local.prefix}.queue_depth{*} > 50"
  message = "${local.monitor}\n\nJob queue depth is elevated — possible processing bottleneck.\n\n${local.notify}"

  monitor_thresholds {
    critical = 50
    warning  = 30
  }

  notify_no_data    = false
  renotify_interval = 15
  tags              = local.tags
}

resource "datadog_monitor" "spot_interruptions" {
  name    = "[runs-fleet] Spot Interruptions Burst"
  type    = "metric alert"
  query   = "sum(last_5m):sum:${local.prefix}.spot_interruptions{*} > 5"
  message = "${local.monitor}\n\nHigh rate of spot instance interruptions.\n\n${local.notify}"

  monitor_thresholds {
    critical = 5
    warning  = 3
  }

  notify_no_data    = false
  renotify_interval = 30
  tags              = local.tags
}

resource "datadog_monitor" "scheduling_failures" {
  name    = "[runs-fleet] Scheduling Failures"
  type    = "metric alert"
  query   = "sum(last_5m):sum:${local.prefix}.scheduling_failure{*} by {task_type} > 3"
  message = "${local.monitor}\n\nScheduling failures detected for task_type:{{task_type.name}}.\n\n${local.notify}"

  monitor_thresholds {
    critical = 3
    warning  = 1
  }

  notify_no_data    = false
  renotify_interval = 15
  tags              = local.tags
}

resource "datadog_monitor" "fleet_size_zero" {
  name    = "[runs-fleet] Fleet Size Zero"
  type    = "metric alert"
  query   = "max(last_5m):max:${local.prefix}.fleet_size{*} <= 0"
  message = "${local.monitor}\n\nFleet size dropped to zero — no instances available.\n\n${local.notify}"

  monitor_thresholds {
    critical = 0
  }

  notify_no_data    = true
  no_data_timeframe = 10
  renotify_interval = 15
  tags              = local.tags
}

resource "datadog_monitor" "job_duration_p95" {
  name    = "[runs-fleet] Job Duration P95 High"
  type    = "metric alert"
  query   = "percentile(last_15m):p95:${local.prefix}.job_duration_seconds{*} > 900"
  message = "${local.monitor}\n\nP95 job duration exceeded 15 minutes.\n\n${local.notify}"

  monitor_thresholds {
    critical = 900
    warning  = 600
  }

  notify_no_data    = false
  renotify_interval = 30
  tags              = local.tags
}

# -----------------------------------------------------------------------------
# Dashboard
# -----------------------------------------------------------------------------

resource "datadog_dashboard" "runs_fleet" {
  title       = "runs-fleet"
  description = "Operational dashboard for the runs-fleet GitHub Actions runner fleet"
  layout_type = "ordered"

  # --- Group 1: Overview ---------------------------------------------------
  widget {
    group_definition {
      layout_type = "ordered"
      title       = "Overview"

      widget {
        query_value_definition {
          title = "Fleet Size"
          request {
            q          = "avg:${local.prefix}.fleet_size{*}"
            aggregator = "last"
          }
          autoscale = true
          precision = 0
          live_span = "5m"
        }
      }

      widget {
        query_value_definition {
          title = "Queue Depth"
          request {
            q          = "avg:${local.prefix}.queue_depth{*}"
            aggregator = "last"
          }
          autoscale = true
          precision = 0
          live_span = "5m"
        }
      }

      widget {
        query_value_definition {
          title = "Jobs Queued (1h)"
          request {
            q          = "sum:${local.prefix}.job_queued{*}.as_count()"
            aggregator = "sum"
          }
          autoscale = true
          precision = 0
          live_span = "1h"
        }
      }

      widget {
        query_value_definition {
          title = "Job Success Rate %"
          request {
            q          = "100 * sum:${local.prefix}.job_success{*}.as_count() / (sum:${local.prefix}.job_success{*}.as_count() + sum:${local.prefix}.job_failure{*}.as_count())"
            aggregator = "avg"
          }
          autoscale  = false
          precision  = 1
          live_span  = "1h"
          custom_unit = "%"
        }
      }
    }
  }

  # --- Group 2: Jobs -------------------------------------------------------
  widget {
    group_definition {
      layout_type = "ordered"
      title       = "Jobs"

      widget {
        timeseries_definition {
          title     = "Job Events"
          live_span = "4h"
          request {
            q            = "sum:${local.prefix}.job_queued{*}.as_count()"
            display_type = "bars"
            style {
              palette = "blue"
            }
          }
          request {
            q            = "sum:${local.prefix}.job_success{*}.as_count()"
            display_type = "bars"
            style {
              palette = "green"
            }
          }
          request {
            q            = "sum:${local.prefix}.job_failure{*}.as_count()"
            display_type = "bars"
            style {
              palette = "red"
            }
          }
        }
      }

      widget {
        heatmap_definition {
          title     = "Job Duration Distribution"
          live_span = "4h"
          request {
            q = "avg:${local.prefix}.job_duration_seconds{*}"
          }
          yaxis {
            include_zero = true
          }
        }
      }

      widget {
        query_value_definition {
          title = "Job Claim Failures (1h)"
          request {
            q          = "sum:${local.prefix}.job_claim_failures{*}.as_count()"
            aggregator = "sum"
          }
          autoscale = true
          precision = 0
          live_span = "1h"
        }
      }
    }
  }

  # --- Group 3: Fleet & Instances ------------------------------------------
  widget {
    group_definition {
      layout_type = "ordered"
      title       = "Fleet & Instances"

      widget {
        timeseries_definition {
          title     = "Fleet Size"
          live_span = "4h"
          request {
            q            = "avg:${local.prefix}.fleet_size{*}"
            display_type = "line"
            style {
              palette    = "dog_classic"
              line_type  = "solid"
              line_width = "normal"
            }
          }
        }
      }

      widget {
        timeseries_definition {
          title     = "Fleet Changes"
          live_span = "4h"
          request {
            q            = "sum:${local.prefix}.fleet_size_increment{*}.as_count()"
            display_type = "bars"
            style {
              palette = "green"
            }
          }
          request {
            q            = "sum:${local.prefix}.fleet_size_decrement{*}.as_count()"
            display_type = "bars"
            style {
              palette = "orange"
            }
          }
        }
      }

      widget {
        timeseries_definition {
          title     = "Spot Interruptions"
          live_span = "4h"
          request {
            q            = "sum:${local.prefix}.spot_interruptions{*}.as_count()"
            display_type = "bars"
            style {
              palette = "warm"
            }
          }
        }
      }

      widget {
        toplist_definition {
          title     = "Circuit Breaker by Instance Type"
          live_span = "1d"
          request {
            q = "top(sum:${local.prefix}.circuit_breaker_triggered{*} by {instance_type}.as_count(), 10, 'sum', 'desc')"
          }
        }
      }

      widget {
        hostmap_definition {
          title    = "Fleet Host Map"
          request {
            fill {
              q = "avg:system.cpu.user{service:runs-fleet} by {host}"
            }
            size {
              q = "avg:system.mem.used{service:runs-fleet} by {host}"
            }
          }
          style {
            palette      = "green_to_orange"
            palette_flip = false
          }
          no_metric_hosts  = false
          no_group_hosts   = true
        }
      }
    }
  }

  # --- Group 4: Queue ------------------------------------------------------
  widget {
    group_definition {
      layout_type = "ordered"
      title       = "Queue"

      widget {
        timeseries_definition {
          title     = "Queue Depth"
          live_span = "4h"
          request {
            q            = "avg:${local.prefix}.queue_depth{*}"
            display_type = "line"
            style {
              palette    = "purple"
              line_type  = "solid"
              line_width = "normal"
            }
          }
        }
      }

      widget {
        timeseries_definition {
          title     = "Message Deletion Failures"
          live_span = "4h"
          request {
            q            = "sum:${local.prefix}.message_deletion_failures{*}.as_count()"
            display_type = "bars"
            style {
              palette = "red"
            }
          }
        }
      }
    }
  }

  # --- Group 5: Pools ------------------------------------------------------
  widget {
    group_definition {
      layout_type = "ordered"
      title       = "Pools"

      widget {
        timeseries_definition {
          title     = "Pool Utilization %"
          live_span = "4h"
          request {
            q            = "avg:${local.prefix}.pool_utilization_percent{*} by {pool_name}"
            display_type = "line"
          }
        }
      }

      widget {
        toplist_definition {
          title     = "Top Pools by Utilization"
          live_span = "1h"
          request {
            q = "top(avg:${local.prefix}.pool_utilization_percent{*} by {pool_name}, 10, 'mean', 'desc')"
          }
        }
      }

      widget {
        timeseries_definition {
          title     = "Warm Pool Hits"
          live_span = "4h"
          request {
            q            = "sum:${local.prefix}.warm_pool_hits{*}.as_count()"
            display_type = "bars"
            style {
              palette = "green"
            }
          }
        }
      }
    }
  }

  # --- Group 6: Cache ------------------------------------------------------
  widget {
    group_definition {
      layout_type = "ordered"
      title       = "Cache"

      widget {
        timeseries_definition {
          title     = "Cache Hits & Misses"
          live_span = "4h"
          request {
            q            = "sum:${local.prefix}.cache_hits{*}.as_count()"
            display_type = "bars"
            style {
              palette = "green"
            }
          }
          request {
            q            = "sum:${local.prefix}.cache_misses{*}.as_count()"
            display_type = "bars"
            style {
              palette = "orange"
            }
          }
        }
      }

      widget {
        query_value_definition {
          title = "Cache Hit Ratio %"
          request {
            q          = "100 * sum:${local.prefix}.cache_hits{*}.as_count() / (sum:${local.prefix}.cache_hits{*}.as_count() + sum:${local.prefix}.cache_misses{*}.as_count())"
            aggregator = "avg"
          }
          autoscale   = false
          precision   = 1
          live_span   = "1h"
          custom_unit = "%"
        }
      }
    }
  }

  # --- Group 7: Housekeeping -----------------------------------------------
  widget {
    group_definition {
      layout_type = "ordered"
      title       = "Housekeeping"

      widget {
        timeseries_definition {
          title     = "Scheduling Failures by Task"
          live_span = "4h"
          request {
            q            = "sum:${local.prefix}.scheduling_failure{*} by {task_type}.as_count()"
            display_type = "bars"
            style {
              palette = "red"
            }
          }
        }
      }

      widget {
        timeseries_definition {
          title     = "Orphaned Instances Terminated"
          live_span = "1d"
          request {
            q            = "sum:${local.prefix}.orphaned_instances_terminated{*}.as_count()"
            display_type = "bars"
            style {
              palette = "warm"
            }
          }
        }
      }

      widget {
        timeseries_definition {
          title     = "Cleanup Operations"
          live_span = "1d"
          request {
            q            = "sum:${local.prefix}.ssm_parameters_deleted{*}.as_count()"
            display_type = "bars"
            style {
              palette = "grey"
            }
          }
          request {
            q            = "sum:${local.prefix}.job_records_archived{*}.as_count()"
            display_type = "bars"
            style {
              palette = "cool"
            }
          }
        }
      }
    }
  }

  # --- Group 8: Logs -------------------------------------------------------
  widget {
    group_definition {
      layout_type = "ordered"
      title       = "Logs"

      widget {
        log_stream_definition {
          query               = "source:runs-fleet"
          columns             = ["core_host", "core_service"]
          show_date_column    = true
          show_message_column = true
          message_display     = "expanded-md"
          sort {
            column = "time"
            order  = "desc"
          }
          live_span = "1h"
        }
      }
    }
  }
}
