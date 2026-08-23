# ビルドは常にランナーのネイティブアーキテクチャで行い、GOARCH だけを対象アーキテクチャに
# 合わせます。最終段をターゲットプラットフォームで組んでもコンパイルはここで済んでいるので、
# QEMU エミュレーションは一切走りません(エミュレーション下のGoビルドは数分かかります)。
FROM --platform=$BUILDPLATFORM golang:1.27 AS build

ARG TARGETOS
ARG TARGETARCH

WORKDIR /src

# 依存の取得を先に済ませ、ソースだけ変わった時にこの層を再利用します。
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO を切って完全な静的バイナリにします。distroless の static イメージには
# libc が無いので、動的リンクだと起動できません。
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/p2pquake-notify .

# static-debian12 はシェルもパッケージマネージャも持たず、CA証明書だけを持ちます。
# タイムゾーンデータは time/tzdata でバイナリに埋め込み済みです。
# プラットフォームは buildx の --platform (ワークフローで linux/arm64) が決めます。
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/p2pquake-notify /p2pquake-notify

USER nonroot:nonroot
ENTRYPOINT ["/p2pquake-notify"]
