job "qol-qwen3-tts-bench" {
  type        = "batch"
  datacenters = ["home"]

  group "bench" {
    restart {
      attempts = 0
      mode     = "fail"
    }

    reschedule {
      attempts  = 0
      unlimited = false
    }

    task "bench" {
      driver = "docker"

      config {
        image      = "docker.io/nvidia/cuda:12.8.1-base-ubuntu24.04"
        entrypoint = ["/bin/bash", "-lc"]
        args = [
          "apt-get update -qq && apt-get install -y -qq python3-venv sox >/dev/null && python3 -m venv /opt/bench && /opt/bench/bin/pip install -q --extra-index-url https://download.pytorch.org/whl/cu128 torch torchaudio qwen-tts && /opt/bench/bin/python $${NOMAD_TASK_DIR}/qwen3_tts_bench.py",
        ]
        shm_size = 4294967296
      }

      template {
        destination     = "local/qwen3_tts_bench.py"
        left_delimiter  = "[[["
        right_delimiter = "]]]"
        data            = file("e2e/bench/qwen3_tts_bench.py")
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
