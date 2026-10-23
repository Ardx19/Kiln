# A small-runner profile for the benchmarks: the compiler the host uses
# (GCC 11.4), Eigen, expat and freetype for RDKit (its ChemDraw library, built by the full build), and ccache for DuckDB, whose build runs
# every compile through /usr/bin/ccache (CCACHE_DISABLE=1 makes it call the
# compiler straight through). The checkout and the conda environment are
# mounted at the same paths as on the host, so compile commands are
# identical. See docs/benchmarks.md.
#
#   docker build -f bench/Dockerfile -t kiln-bench bench
FROM ubuntu:22.04
RUN apt-get update \
 && apt-get install -y --no-install-recommends g++ libeigen3-dev libexpat1-dev libfreetype-dev ccache ca-certificates \
 && rm -rf /var/lib/apt/lists/*
