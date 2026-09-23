import vue from "@vitejs/plugin-vue";

export default {
  plugins: [vue()],
  base: "/ui/",
  build: {
    // go:embed 在 cmd/ask/webui.go 指向 cmd/ask/web/dist
    outDir: "../cmd/ask/web/dist",
    emptyOutDir: true,
  },
};
