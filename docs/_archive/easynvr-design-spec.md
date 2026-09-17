# EasyNVR 设计规范（抓取结果）

## 技术栈
- **Ant Design v5** + **Tailwind CSS**
- 字体：`PingFang SC, Microsoft YaHei, Helvetica Neue, Arial, sans-serif`
- 基准字号：14px

## 配色（按使用频率）
| 颜色 | 用途 |
|---|---|
| `#fff` | 卡片背景、文字 |
| `#f5f5f5` | 页面背景 |
| `#1677ff` | **主色**（Ant Design v5 默认蓝） |
| `#1177bb` | 链接/hover |
| `#d93025` | 危险/离线 |
| `#29ab02` | 成功/在线 |
| `#2f2f2f` | 深色文字 |
| `#242c3d` | 侧边栏深色 |
| `#7b7b7b` `#616c72` | 次要文字 |
| `#d9d9d9` `#ccc` | 边框 |

## 圆角
- 3px（小元素）
- 8px（卡片）
- 24px（标签/胶囊）

## 布局
- 侧边栏：深色 `#001529`（Ant Design Pro 默认），宽 208px
- 顶栏：白色，高 64px
- 内容区：`#f5f5f5` 背景，24px 内边距

## 登录页结构
```
h-screen bg-cover
  py-8 relative top-[40%] left-1/2 -translate-y-1/2 -translate-x-1/2
    form ant-form (350px 宽)
      h3 "EasyNVR"
      ant-input-affix-wrapper-lg  (带图标前缀)
      ant-btn-primary
```

## 关键视觉特征
1. 卡片：白底 + 8px 圆角 + 极轻阴影 `0 1px 2px rgba(0,0,0,.03)`
2. 表格：表头浅灰底，行 hover 变色
3. 按钮：主按钮实心蓝，次按钮描边
4. 输入框：大尺寸（lg），左侧图标前缀
5. 标签：胶囊形，浅色背景 + 深色文字
