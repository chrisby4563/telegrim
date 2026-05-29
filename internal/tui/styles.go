package tui

import "github.com/charmbracelet/lipgloss"

var (
	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#89b4fa"))

	mutedStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#6c7086"))

	selectedStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#f9e2af")).
			Background(lipgloss.Color("#313244"))

	selectedMetaStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#bac2de")).
				Background(lipgloss.Color("#313244"))

	textStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#cdd6f4"))

	outgoingStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#a6e3a1"))

	errorStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#f38ba8"))

	closeStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#ffffff")).
			Background(lipgloss.Color("#b64141"))

	sendStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#1e1e2e")).
			Background(lipgloss.Color("#a6e3a1"))

	tabStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#bac2de"))

	tabActiveStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#1e1e2e")).
			Background(lipgloss.Color("#89b4fa"))

	loginStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#1e1e2e")).
			Background(lipgloss.Color("#f9e2af"))

	qrStyle = lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("#1e1e2e")).
		Background(lipgloss.Color("#cba6f7"))

	refreshStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#1e1e2e")).
			Background(lipgloss.Color("#94e2d5"))

	splitterStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#585b70"))

	splitterActiveStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#89b4fa")).
				Bold(true)

	contactSepStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#313244"))

	avatarBgStyle = lipgloss.NewStyle().
			Background(lipgloss.Color("#1e1e2e"))

	onlineDotStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#43d17e")).
			Bold(true)

	readTickStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#89b4fa")).
			Bold(true)

	focusBarStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#89b4fa")).
			Bold(true)
	focusBarWAStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#25d366")).
			Bold(true)
	idleBarStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#45475a"))

	// Catppuccin Mocha + Telegram-ähnliches Outgoing-Blau.
	bubbleIncoming = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#cdd6f4")).
			Background(lipgloss.Color("#313244")).
			Padding(0, 1).
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("#313244"))

	bubbleOutgoing = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#eef3fb")).
			Background(lipgloss.Color("#2f6cb5")).
			Padding(0, 1).
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("#2f6cb5"))

	// WhatsApp-Variante: dunkleres WA-Grün analog Original-WA-App.
	bubbleOutgoingWA = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#e7f7ec")).
				Background(lipgloss.Color("#1f7a45")).
				Padding(0, 1).
				Border(lipgloss.RoundedBorder()).
				BorderForeground(lipgloss.Color("#1f7a45"))

	// Selected-Varianten heben die aktive Nachricht durch heller leuchtenden
	// Rand hervor (Catppuccin Peach/Yellow).
	bubbleIncomingSelected = bubbleIncoming.
				BorderForeground(lipgloss.Color("#f9e2af"))

	bubbleOutgoingSelected = bubbleOutgoing.
				BorderForeground(lipgloss.Color("#f9e2af"))

	bubbleOutgoingWASelected = bubbleOutgoingWA.
					BorderForeground(lipgloss.Color("#f9e2af"))

	bubbleSenderStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#89b4fa")).
				Bold(true)

	mediaLineStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#fab387")).
			Italic(true)

	bubbleMetaIncoming = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#7f849c")).
				Background(lipgloss.Color("#313244"))

	bubbleMetaOutgoing = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#b6d3f0")).
				Background(lipgloss.Color("#2f6cb5"))

	bubbleMetaOutgoingWA = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#b6e8c8")).
				Background(lipgloss.Color("#1f7a45"))

	dateSeparatorStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#bac2de")).
				Background(lipgloss.Color("#313244")).
				Padding(0, 2).
				Bold(true)

	chatHeaderName = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#cdd6f4")).
			Bold(true)

	chatHeaderMeta = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#7f849c"))
)
