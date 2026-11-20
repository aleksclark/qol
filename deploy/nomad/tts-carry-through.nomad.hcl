variable "processor" {
  type        = string
  description = "TTS processor script under e2e/processors"
}

variable "packages" {
  type        = string
  description = "pip requirements for the processor"
}

variable "fixture" {
  type        = string
  description = "Synthesis fixture written by the real graph test (QOL_E2E_DUMP_SYNTHESIS)"
}

variable "torch_index" {
  type    = string
  default = "https://download.pytorch.org/whl/cu126"
}

variable "environment" {
  type    = map(string)
  default = {}
}

job "qol-tts-carry-through" {
  type        = "batch"
  datacenters = ["home"]

  group "tts" {
    restart {
      attempts = 0
      mode     = "fail"
    }

    reschedule {
      attempts  = 0
      unlimited = false
    }

    task "tts" {
      driver = "docker"

      config {
        image      = "docker.io/nvidia/cuda:12.8.1-base-ubuntu24.04"
        entrypoint = ["/bin/bash", "-lc"]
        args       = ["$${NOMAD_TASK_DIR}/run.sh"]
        shm_size   = 4294967296
        volumes    = ["qol-python-cache:/root/.cache"]
      }

      env = merge({ QOL_PROCESSOR = var.processor }, var.environment)

      template {
        destination = "local/run.sh"
        perms       = "0755"
        data        = <<-EOT
          set -euo pipefail
          apt-get update -qq
          apt-get install -y -qq python3-venv ffmpeg sox >/dev/null
          python3 -m venv /opt/tts
          /opt/tts/bin/pip install -q --extra-index-url ${var.torch_index} ${var.packages} 'protobuf>=7.36.1,<8' scipy
          base64 -d "$NOMAD_TASK_DIR/requests.b64" > "$NOMAD_TASK_DIR/requests.bin"
          cd "$NOMAD_TASK_DIR/processors"
          nvidia-smi --query-gpu=name,memory.total --format=csv,noheader
          start=$(date +%s.%N)
          /opt/tts/bin/python "$QOL_PROCESSOR" < "$NOMAD_TASK_DIR/requests.bin" > "$NOMAD_TASK_DIR/audio.bin"
          end=$(date +%s.%N)
          echo "synthesis wall seconds: $(echo "$end - $start" | bc 2>/dev/null || python3 -c "print($end-$start)")"
          ls -l "$NOMAD_TASK_DIR/audio.bin"
        EOT
      }

      template {
        destination     = "local/requests.b64"
        left_delimiter  = "[[["
        right_delimiter = "]]]"
        data            = filebase64(var.fixture)
      }

      template {
        destination     = "local/processors/qol_processor.py"
        left_delimiter  = "[[["
        right_delimiter = "]]]"
        data            = file("e2e/processors/qol_processor.py")
      }

      template {
        destination     = "local/processors/${var.processor}"
        left_delimiter  = "[[["
        right_delimiter = "]]]"
        data            = file("e2e/processors/${var.processor}")
      }

      template {
        destination     = "local/processors/gen/qol/v1/qol_pb2.py"
        left_delimiter  = "[[["
        right_delimiter = "]]]"
        data            = file("e2e/processors/gen/qol/v1/qol_pb2.py")
      }

      template {
        destination     = "local/processors/gen/qol/v1/processor_pb2.py"
        left_delimiter  = "[[["
        right_delimiter = "]]]"
        data            = file("e2e/processors/gen/qol/v1/processor_pb2.py")
      }

      resources {
        cpu    = 4000
        memory = 16384

        device "nvidia/gpu" {
          count = 1

          constraint {
            attribute = "$${device.model}"
            value     = "Tesla V100-SXM2-16GB"
          }
        }
      }
    }
  }
}
