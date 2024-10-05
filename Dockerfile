FROM scratch

ARG COMMIT_SHA
ARG REPO_URL

WORKDIR /

COPY jenkins-client /

LABEL org.opencontainers.image.commit.ref=$COMMIT_SHA
LABEL org.opencontainers.image.repo.url=$REPO_URL

ENTRYPOINT ["/jenkins-client"]
