# Deployment Guide

This guide describes how to install and run **Quartermaster** on a Debian-based system.

## 📋 Prerequisites

- **containerd**: The runtime used for managing containers.
- **git**: For the GitOps-based configuration.
- **Go (for building)**: If you are building from source.
- **A dedicated user**: It is highly recommended to run Quartermaster under a dedicated non-root user with appropriate permissions.

## 🛠️ Installation

### 1. Build the binaries

From the root of the repository:

```bash
make build
```

This will produce the following binaries in the `bin/` directory:
- `qm`: The command-line interface.
- `qm-daemon`: The background reconciliation daemon.

### 2. Install the binaries

Move the binaries to a location in your PATH (e.g., `/usr/local/bin`):

```bash
sudo cp bin/qm /usr/local/bin/
sudo cp bin/qm-daemon /usr/local/bin/
```

### 3. Set up the configuration directory

Create the directory where Quartermaster will look for its configuration:

```bash
sudo mkdir -p /etc/quartermaster
```

Create a configuration file at `/etc/quartermaster/config.yaml`. An example configuration is provided in the `samples/` directory (though those are stack files, you might need a global config).

*Note: You may need to define your global configuration schema in `pkg/config` if not already implemented.*

### 4. Create the dedicated user

For security, create a `quartermaster` user:

```bash
sudo useradd -m -s /usr/sbin/nologin quartermaster
```

Ensure this user has access to the `containerd` socket (usually `/run/containerd/containerd.sock`). You might need to add the user to a specific group or use `setfacl`.

```bash
sudo setfacl -m u:quartermaster:rw /run/containerd/containerd.sock
```

## 🚀 Running Quartermaster

### Running manually (for testing)

You can run the daemon manually in the foreground to see the logs:

```bash
sudo qm-daemon --config /etc/quartermaster/config.yaml
```

### Running as a systemd service

To run Quartermaster as a background service, use the provided systemd unit template.

1. Copy the template to `/etc/systemd/system/`:

```bash
sudo cp docs/systemd/qm-daemon.service /etc/systemd/system/
```

2. Edit `/etc/systemd/system/qm-daemon.service` to ensure the `ExecStart` path and configuration path are correct.

3. Reload systemd and start the service:

```bash
sudo systemctl daemon-reload
sudo systemctl enable qm-daemon
sudo systemctl start qm-daemon
```

4. Check the status:

```bash
sudo systemctl status qm-daemon
```

## 📖 Basic Usage

Once running, use the `qm` CLI to interact with the system.

```bash
# List current services (if implemented)
qm list

# Check status of the daemon
qm status
```

## 🔍 Troubleshooting

If the daemon fails to start, check the system logs:

```bash
journalctl -u qm-daemon -f
```
