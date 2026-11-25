# L4 Load Balancer (Track C)

This project contains the traffic generator, switch, and server placeholders to emulate multi-region load testing of an L4 Load Balancer.

## Usage

1. Build and run via Docker Compose:
```bash
docker-compose up --build
```
2. The `trafficgen` container will run for `60s` (configurable in `config.yaml`) and output the traffic summary directly to the standard output.
3. Once completed, `trafficgen` stops. The output CSVs will be available in the `results/` folder:
    - `results/gen.csv` - Traffic generation metrics
    - `results/server.csv` - Requests received by the server

## Components

- **Traffic Generator (`cmd/trafficgen`)**: Reads the `config.yaml`, binds multiple IP aliases per region, and fires requests (TCP, UDP, HTTP) respecting a Zipf key distribution and Poisson arrival rate.
- **Switch (`cmd/switch`)**: Intercepts requests, adds latency and loss based on the client's source IP subnet, and prefixes requests with `PROXY` header for upstream server.
- **Server (`cmd/server`)**: A simple echo server substituting the future L4 Load Balancer implementation. Records metrics to `server.csv`.
