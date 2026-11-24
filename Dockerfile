# contelys/edge: the official APISIX image of the test baseline, pinned at its digest (Edge detailed
# design, section 08). It adds no plugin and no code.
FROM apache/apisix:3.21.0-debian@sha256:1741a8e1186e5c39ea10e6b4fb3e682534d1705efb8328195de5b41ad0657380

# The default config.yaml, which the image scan uses. Each instance mounts the config.yaml that the
# renderer writes for its installation in its place.
COPY --chown=apisix:root config.yaml /usr/local/apisix/conf/config.yaml

# APISIX reads its routes only from conf/apisix.yaml. The link points into /etc/edge/routes, where each
# instance mounts the directory that holds its apisix.yaml, so that a file renamed into place there is
# applied without a restart (Edge ADR 0003). /etc/edge also receives the trust store, ca.crt.
USER root
RUN mkdir -p /etc/edge/routes && ln -s /etc/edge/routes/apisix.yaml /usr/local/apisix/conf/apisix.yaml
USER apisix
