job "sleeper" {
  datacenters = ["dc1"]
  task "sleeper" {
    driver = "raw_exec"
    config {
      command = "/bin/bash"
      args    = ["-c", "echo $$ >> pid && sleep 999999"]
    }
  }
}
