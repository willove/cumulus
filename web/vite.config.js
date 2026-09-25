import vue from "@vitejs/plugin-vue";

export default {
  plugins: [vue()],
  base: "/ui/",
  server: {
    proxy: {
      "/v1": process.env.CUL_API_TARGET || "http://127.0.0.1:8484",
    },
  },
  build: {
    // go:embed 在 cmd/cumulus-cluster/webui.go 指向 cmd/cumulus-cluster/web/dist
    outDir: "../cmd/cumulus-cluster/web/dist",
    emptyOutDir: true,
  },
};
