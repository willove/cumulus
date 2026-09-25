import { createApp } from "vue";
import EvokeBusinessUI from "@wil-works/evoke-business-ui";
import "@wil-works/evoke-business-ui/styles";
import EvokeChat from "@wil-works/evoke-chat";
import "@wil-works/evoke-chat/styles";
import "./theme.css";
// 图表样式不在 business-ui 的产物里（evoke-charts.css 里才有 .ev-chart），
// 少这一行图表就是不可见（canvas 绝对定位 + 容器无高度）。<eb-chart> 组件本身
// 已由 business-ui 全局注册，所以这里只需要补样式。
import "@wil-works/evoke-charts/styles";
import App from "./App.vue";

createApp(App).use(EvokeBusinessUI).use(EvokeChat).mount("#app");
