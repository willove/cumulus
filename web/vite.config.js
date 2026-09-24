import vue from "@vitejs/plugin-vue";

export default {
  plugins: [vue()],
  base: "/ui/",
  build: {
    // go:embed 在 cmd/cumulus-cluster/webui.go 指向 cmd/cumulus-cluster/web/dist
    outDir: "../cmd/cumulus-cluster/web/dist",
    emptyOutDir: true,
  },
};
