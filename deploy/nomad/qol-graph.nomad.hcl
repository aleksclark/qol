variable "image_go_worker" {
  type        = string
  description = "Digest-pinned qol go-worker image (capture, record)"
}

variable "image_stt" {
  type = string
}

variable "image_prosody" {
  type = string
}

variable "image_translate" {
  type = string
}

variable "image_tts" {
  type = string
}

variable "node" {
  type        = string
  default     = "node-1"
  description = "Node that runs the CPU graph; stream state is process-local, so each stage runs once"
}

variable "output_dir" {
  type    = string
  default = "/mnt/moosefs/configs/qol/recordings"
}

locals {
  nats_env = <<-EOT
    QOL_NATS_URL={{ range $index, $service := nomadService "nats" }}{{ if $index }},{{ end }}nats://{{ $service.Address }}:{{ $service.Port }}{{ end }}
    QOL_NATS_USER=qol
    QOL_NATS_PASSWORD={{ with nomadVar "nomad/jobs/qol-graph" }}{{ .nats_password }}{{ end }}
  EOT
}

job "qol-graph" {
  type        = "service"
  datacenters = ["home"]

  meta {
    project = "qol"
    source  = "https://git.clark.team/aleksclark/qol/src/branch/master/deploy/nomad/qol-graph.nomad.hcl"
  }

  constraint {
    attribute = "${node.unique.name}"
    value     = var.node
  }

  update {
    max_parallel     = 1
    health_check     = "task_states"
    min_healthy_time = "10s"
    healthy_deadline = "5m"
    auto_revert      = false
    canary           = 0
  }

  group "capture" {
    task "capture" {
      driver = "docker"

      config {
        image = var.image_go_worker
        args  = ["--type", "capture"]
      }

      identity {
        name = "default"
        aud  = ["nomadproject.io"]
      }

      template {
        destination          = "secrets/nats.env"
        env                  = true
        change_mode          = "restart"
        error_on_missing_key = true
        data                 = local.nats_env
      }

      resources {
        cpu    = 100
        memory = 64
      }
    }
  }

  group "stt" {
    task "stt" {
      driver = "docker"

      config {
        image = var.image_stt
        args  = ["--type", "stt-whisper"]
      }

      identity {
        name = "default"
        aud  = ["nomadproject.io"]
      }

      template {
        destination          = "secrets/nats.env"
        env                  = true
        change_mode          = "restart"
        error_on_missing_key = true
        data                 = local.nats_env
      }

      resources {
        cpu    = 2000
        memory = 1024
      }
    }
  }

  group "prosody" {
    task "prosody" {
      driver = "docker"

      config {
        image = var.image_prosody
        args  = ["--type", "prosody"]
      }

      identity {
        name = "default"
        aud  = ["nomadproject.io"]
      }

      template {
        destination          = "secrets/nats.env"
        env                  = true
        change_mode          = "restart"
        error_on_missing_key = true
        data                 = local.nats_env
      }

      resources {
        cpu        = 1500
        memory     = 2048
        memory_max = 3072
      }
    }
  }

  group "translate" {
    task "translate" {
      driver = "docker"

      config {
        image = var.image_translate
        args  = ["--type", "translate"]
      }

      identity {
        name = "default"
        aud  = ["nomadproject.io"]
      }

      template {
        destination          = "secrets/nats.env"
        env                  = true
        change_mode          = "restart"
        error_on_missing_key = true
        data                 = local.nats_env
      }

      resources {
        cpu        = 2000
        memory     = 1536
        memory_max = 2048
      }
    }
  }

  group "tts" {
    task "tts" {
      driver = "docker"

      config {
        image = var.image_tts
        args  = ["--type", "tts"]
      }

      identity {
        name = "default"
        aud  = ["nomadproject.io"]
      }

      template {
        destination          = "secrets/nats.env"
        env                  = true
        change_mode          = "restart"
        error_on_missing_key = true
        data                 = local.nats_env
      }

      resources {
        cpu        = 3000
        memory     = 2048
        memory_max = 3072
      }
    }
  }

  dynamic "group" {
    for_each = {
      "record-en" = { input = "graph.record-en.input", output = "graph.record-en.completed" }
      "record-es" = { input = "graph.record-es.input", output = "graph.record-es.completed" }
    }
    labels = [group.key]

    content {
      task "record" {
        driver = "docker"

        config {
          image   = var.image_go_worker
          args    = ["--type", "record", "--name", group.key, "--input", group.value.input, "--output", group.value.output, "--output-dir", "/output"]
          volumes = ["${var.output_dir}:/output"]
        }

        identity {
          name = "default"
          aud  = ["nomadproject.io"]
        }

        template {
          destination          = "secrets/nats.env"
          env                  = true
          change_mode          = "restart"
          error_on_missing_key = true
          data                 = local.nats_env
        }

        resources {
          cpu    = 300
          memory = 128
        }
      }
    }
  }
}
