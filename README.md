# ClusterBulb 💡

A Kubernetes health monitor that talks through light.

ClusterBulb is a small Go-based monitoring service that turns your Kubernetes cluster’s health into a visual signal using a smart bulb connected to Home Assistant.
It watches cluster nodes, pods, and warning events, checks GitHub for open PRs, and updates a Home Assistant light entity to reflect the overall state.

![Myrmidon Soldier Holding a Light Bulb](https://raw.githubusercontent.com/clustershed/images/refs/heads/main/myrmidon-bulb-anim.webp)

# 🎯 Overview

| ---------------------: | ------------------------------------------------------------------- |
|             🟢 Green	 | Cluster is healthy               |
|             🔵 Blue	 | Open GitHub pull requests               |
|             🔴 Red	 | Detected issues in cluster               |
| 🔴🔵 Blinking Red/Blue | Blinking Red/Blue	Both open PRs and detected issues              |

ClusterBulb is ambient observability. A simple, physical indicator of cluster state.

# 🧠 How it works

- Runs as a non-root pod inside Kubernetes (in-cluster K8s client).
- Monitors Nodes, Pods, and Warning Events using the Kubernetes API.
- Polls GitHub for open PRs (configurable interval).
- Sends color/brightness commands to the Home Assistant REST API to update a light entity.
- Maintains minimal permissions (read-only) via RBAC.

# 🔧 Configuration / Environment variables

|               Variable | Meaning                                                             |
| ---------------------: | ------------------------------------------------------------------- |
|             `HA_TOKEN` | Home Assistant long-lived access token (from Secrets)               |
|               `HA_URL` | Base URL of Home Assistant (e.g. `http://homeassistant.local:8123`) |
|   `HA_LIGHT_ENTITY_ID` | Home Assistant light entity id (e.g. `light.cluster_bulb`)          |
|  `HA_LIGHT_BRIGHTNESS` | Brightness (1–255, default 255)                                     |
|             `GH_OWNER` | GitHub owner (user/org)                                             |
|              `GH_REPO` | GitHub repository name                                              |
|             `GH_TOKEN` | GitHub token (optional but recommended to avoid rate limits)        |
| `GH_PR_CHECK_INTERVAL` | Seconds between PR checks (default 300)                             |

Secrets `HA_TOKEN` and `GH_TOKEN` should be provided via a Kubernetes Secret named clusterbulb-secrets.

# 🛡 Security notes

- The binary exits if run as root (UID 0).
- Pod runs as non-root (runAsUser: 1000, fsGroup: 1000).
- allowPrivilegeEscalation: false is set.
- RBAC is read-only for the core and apps API groups.
- Secrets are consumed via valueFrom: secretKeyRef.
- Do not store tokens in plaintext in your repository!



