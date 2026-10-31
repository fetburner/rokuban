# 派生物の置き場（S3 互換）の MinIO イメージ。**ビルドはこの Dockerfile の外で行う**
# （lib/cluster.sh の build_minio_image が、.github/actions/build-minio と同じ commit の
# ソースを `go build` して、バイナリとこの Dockerfile だけを入れた一時ディレクトリを
# context にする）。公式の MinIO イメージは公開されていないので、ビルドしか道がない。
#
# `scratch` なのでシェルは無い。Pod 側は readOnlyRootFilesystem なので、書けるのは
# emptyDir（/data と /tmp）だけである。
FROM scratch
COPY minio /minio
EXPOSE 9000
ENTRYPOINT ["/minio"]
