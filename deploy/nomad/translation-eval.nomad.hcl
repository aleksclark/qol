variable "models" {
  type        = string
  description = "Comma-separated models accepted by e2e/bench/translation_eval.py"
}

variable "limit" {
  type    = number
  default = 300
}

variable "dtype" {
  type    = string
  default = "float16"
}

job "qol-translation-eval" {
  type        = "batch"
  datacenters = ["home"]

  group "eval" {
    restart {
      attempts = 0
      mode     = "fail"
    }

    reschedule {
      attempts  = 0
      unlimited = false
    }

    task "eval" {
      driver = "docker"

      config {
        image      = "docker.io/nvidia/cuda:12.8.1-base-ubuntu24.04"
        entrypoint = ["/bin/bash", "-lc"]
        args       = ["$${NOMAD_TASK_DIR}/run.sh"]
        shm_size   = 4294967296
        volumes    = ["qol-python-cache:/root/.cache"]
      }

      env {
        QOL_EVAL_DTYPE  = var.dtype
        QOL_PUNCT_MODEL = "/opt/punct/sherpa-onnx-online-punct-en-2024-08-06"
      }

      template {
        destination = "local/run.sh"
        perms       = "0755"
        data        = <<-EOT
          set -euo pipefail
          apt-get update -qq
          apt-get install -y -qq python3-venv curl bzip2 >/dev/null
          python3 -m venv /opt/eval
          /opt/eval/bin/pip install -q --extra-index-url https://download.pytorch.org/whl/cu126 torch==2.8.0 transformers==4.57.6 accelerate sentencepiece sacrebleu sacremoses sherpa-onnx ctranslate2==4.8.1 huggingface-hub==0.36.0
          mkdir -p /opt/punct
          curl -fsSL https://github.com/k2-fsa/sherpa-onnx/releases/download/punctuation-models/sherpa-onnx-online-punct-en-2024-08-06.tar.bz2 | tar -xjf - -C /opt/punct
          export PATH=/opt/eval/bin:$PATH
          python -u "$NOMAD_TASK_DIR/translation_eval.py" --models '${var.models}' --limit ${var.limit}
        EOT
      }

      template {
        destination     = "local/translation_eval.py"
        left_delimiter  = "[[["
        right_delimiter = "]]]"
        data            = file("e2e/bench/translation_eval.py")
      }

      resources {
        cpu    = 4000
        memory = 24576

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
