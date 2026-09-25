import { createApp } from "vue";
import EvokeBusinessUI from "@wil-works/evoke-business-ui";
import "@wil-works/evoke-business-ui/styles";
import EvokeChat from "@wil-works/evoke-chat";
import "@wil-works/evoke-chat/styles";
import "./theme.css";
import App from "./App.vue";

createApp(App).use(EvokeBusinessUI).use(EvokeChat).mount("#app");
