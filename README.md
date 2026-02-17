# Xavi

Xavi is a lightweight, secure edge infrastructure agent written in Go. It manages the lifecycle of local containers (Postgres, Valkey, App, Caddy, BackupBot) and coordinates with other Xavi nodes to form a distributed mesh using a gossip protocol.

## Features

-   **Container Management**: Automatically pulls, starts, and configures Docker containers.
-   **Resource Limits**: Enforces CPU and Memory limits on managed services.
-   **Security**:
    -   Host-specific secret generation (Postgres password, Valkey password, Cluster key).
    -   Encrypted Gossip communication.
    -   Secure service-to-service authentication.
-   **Clustering**:
    -   UDP Gossip via `hashicorp/memberlist`.
    -   Service Discovery (e.g., App finds remote Postgres).
    -   Shared secret cluster authorization.
-   **Configuration**:
    -   Hot-reloading from `/etc/tripleclabs/xavi.json`.
    -   CLI-based initial auth bundle.

## Getting Started

### Prerequisites

-   Podman or Docker installed and running.
-   Linux (AMD64/ARM64).

### Installation

The install script detects your architecture, downloads the binary with checksum verification, and sets up a systemd service. It will use an existing Podman or Docker installation, or offer to install Podman if neither is found.

**Interactive:**

```bash
curl -fsSL https://raw.githubusercontent.com/tripleclabs/xavi/main/install.sh | bash
```

**Unattended (auto-accepts Podman install if needed):**

```bash
curl -fsSL https://raw.githubusercontent.com/tripleclabs/xavi/main/install.sh | bash -s -- -y
```

### First Run

For the first run, you can provide an initial base64-encoded configuration bundle to bootstrap the agent:

```bash
sudo xavi --auth <BASE64_BUNDLE>
```

Or ensure `/etc/tripleclabs/xavi.json` exists.

## Configuration

The configuration file is located at `/etc/tripleclabs/xavi.json`.

### Example `xavi.json`

```json
{
  "control": {
    "url": "https://api.example.com",
    "interval": 30
  },
  "docker": {
    "registry": "docker.io",
    "username": "myuser",
    "password": "mypassword"
  },
  "images": {
    "app": "myorg/app:latest",
    "valkey": "valkey/valkey:8",
    "postgres": "postgres:16",
    "backupbot": "wearecococo/backupbot:latest"
  },
  "valkey": {
    "max_ram": "512m",
    "max_cpu": "0.5"
  },
  "postgres": {
    "max_ram": "1g",
    "max_cpu": "1.0",
    "storage_path": "/mnt/data/postgres"
  },
  "cluster": {
    "bind_port": 7946,
    "peers": ["192.168.1.10:7946", "192.168.1.11:7946"]
  },
  "services": ["app", "valkey"] 
}
```

-   `services`: List of services enabled on *this* specific node. If omitted, all services are enabled.
-   `cluster.peers`: List of seed nodes to join the gossip mesh.

## Security Model

Xavi relies on a **shared secret** model for cluster security.

### Secrets File
On the first run, Xavi generates a secrets file at `/etc/tripleclabs/xavi.secrets` with `0600` permissions.

```json
{
  "postgres_password": "...",
  "postgres_db_name": "pulse",
  "postgres_db_user": "pulse",
  "replication_password": "...",
  "valkey_password": "...",
  "cluster_key": "...",
  "app_encryption_key": "...",
  "app_token_secrets": ["..."]
}
```

-   **Postgres Credentials**: Password, database name, and user — injected into the Postgres container and connection URLs.
-   **Valkey Password**: Injected into the Valkey container and app configuration.
-   **Cluster Key**: A 32-byte base64 key used to encrypt all gossip traffic.
-   **App Encryption Key / Token Secrets**: Injected into the app configuration for application-level encryption and token signing.

### ⚠️ Important: Multi-Node Setup
To form a secure cluster, **you must copy the `xavi.secrets` file from the first node to all other nodes**. 

If nodes have different `cluster_key`s, they will **not** be able to communicate.

## Service Discovery

Xavi nodes broadcast their enabled services to the cluster.
-   If `app` is running on Node A and `postgres` is on Node B:
    -   Node A discovers Node B via gossip.
    -   Node A configures the `app` container with `POSTGRES_HOST=<Node B IP>`.
-   If `postgres` is running locally (on Node A), `POSTGRES_HOST` is set to `xavi-postgres`.

## License

Proprietary / Internal.
