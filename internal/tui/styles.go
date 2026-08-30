package tui

import "github.com/charmbracelet/lipgloss"

var (
	// 品牌配色
	brandPurple = lipgloss.Color("99")
	pinkText    = lipgloss.Color("206")
	dimText     = lipgloss.Color("242")
	mutedText   = lipgloss.Color("245")
	normalText  = lipgloss.Color("252")
	brightText  = lipgloss.Color("255")
	greenText   = lipgloss.Color("78")
	redText     = lipgloss.Color("203")
	yellowText  = lipgloss.Color("214")
	cyanText    = lipgloss.Color("80")

	// 横幅
	bannerStyle = lipgloss.NewStyle().
			Foreground(pinkText).
			Bold(true)

	bannerDimStyle = lipgloss.NewStyle().
			Foreground(dimText)

	// 分隔线
	separatorStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("236"))

	// 用户提示标记
	promptStyle = lipgloss.NewStyle().
			Foreground(cyanText).
			Bold(true)

	// AI 回复标记
	aiMarkerStyle = lipgloss.NewStyle().
			Foreground(brandPurple).
			Bold(true)

	// AI 文本
	aiTextStyle = lipgloss.NewStyle().
			Foreground(normalText).
			PaddingLeft(2)

	// 流式文本（流式输出时略微变暗）
	streamingTextStyle = lipgloss.NewStyle().
				Foreground(normalText).
				PaddingLeft(2)

	// 工具调用样式

	toolRunningStyle = lipgloss.NewStyle().
				Foreground(dimText).
				PaddingLeft(2)

	toolDoneStyle = lipgloss.NewStyle().
			Foreground(greenText).
			PaddingLeft(2)

	toolErrorStyle = lipgloss.NewStyle().
			Foreground(redText).
			PaddingLeft(2)

	toolDetailStyle = lipgloss.NewStyle().
			Foreground(dimText).
			PaddingLeft(4)

	// EditFile diff 行样式：新增绿、删除红，上下文行复用 toolDetailStyle
	diffAddStyle = lipgloss.NewStyle().
			Foreground(greenText).
			PaddingLeft(4)

	diffRemoveStyle = lipgloss.NewStyle().
			Foreground(redText).
			PaddingLeft(4)

	// 错误消息
	errorStyle = lipgloss.NewStyle().
			Foreground(redText).
			PaddingLeft(2)

	// 权限对话框
	permBorderStyle = lipgloss.NewStyle().
			Foreground(yellowText).
			Bold(true)

	permDimStyle = lipgloss.NewStyle().
			Foreground(dimText)

	// 状态栏（底部）
	statusBarStyle = lipgloss.NewStyle().
			Foreground(dimText)

	statusItemStyle = lipgloss.NewStyle().
			Foreground(mutedText)

	// Provider 选择
	selectLabelStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(brandPurple).
				Align(lipgloss.Center)

	selectedItemStyle = lipgloss.NewStyle().
				Foreground(cyanText).
				Bold(true)

	normalItemStyle = lipgloss.NewStyle().
			Foreground(mutedText)
)
