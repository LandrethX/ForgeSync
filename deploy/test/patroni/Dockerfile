# PostgreSQL under Patroni, for the database-failover check.
#
# The test environment's ordinary database (forgesync-db) is one container
# and one copy. This image is the other arrangement deploy/prod/README.md
# points at: two PostgreSQL servers in streaming replication with a
# promotion tool over them, so the database survives losing one. It is the
# stock postgres image with Patroni added; nothing about ForgeSync is in it.
ARG POSTGRES_VERSION
FROM postgres:${POSTGRES_VERSION}

ARG PATRONI_VERSION
RUN apt-get update \
 && apt-get install -y --no-install-recommends python3 python3-venv \
 && rm -rf /var/lib/apt/lists/* \
 && python3 -m venv /opt/patroni \
 && /opt/patroni/bin/pip install --no-cache-dir \
      "patroni[etcd3]==${PATRONI_VERSION}" "psycopg[binary]"

ENV PATH=/opt/patroni/bin:$PATH

COPY patroni.yml /etc/patroni/patroni.yml
COPY post-init.sh /usr/local/bin/patroni-post-init
COPY entrypoint.sh /usr/local/bin/patroni-entrypoint
RUN chmod +x /usr/local/bin/patroni-post-init /usr/local/bin/patroni-entrypoint

EXPOSE 5432 8008

# The same readiness compose watches. PostgreSQL is what this container is
# for, so a container whose PostgreSQL isn't answering is not healthy,
# whichever of the two it currently is. pg_isready already exits non-zero
# when it isn't, and the user is the one compose sets.
HEALTHCHECK --interval=10s --timeout=5s --start-period=30s --retries=6 \
  CMD ["pg_isready", "-U", "forgesync", "-d", "postgres"]

# The entrypoint starts as root only to take ownership of the data volume,
# then execs Patroni as the postgres user (gosu, as the postgres image
# itself does). A scanner reads the missing USER here as "runs as root";
# the long-running process is uid 999, which `gosu postgres id` confirms.
ENTRYPOINT ["/usr/local/bin/patroni-entrypoint"]
