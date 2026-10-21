# A small-runner profile for the RDKit benchmark: the compiler the host
# uses (GCC 11.4) and Eigen, with the checkout and the conda environment
# mounted at the same paths as on the host, so compile commands are
# identical. See docs/benchmarks.md.
#
#   docker build -f bench/Dockerfile -t kiln-bench bench
FROM ubuntu:22.04
RUN apt-get update \
 && apt-get install -y --no-install-recommends g++ libeigen3-dev ca-certificates \
 && rm -rf /var/lib/apt/lists/*
