package main

import (
	"flag"
	"log"
	"os"
	"os/signal"
	"runtime/pprof"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/joho/godotenv"
	"layeh.com/gumble/gumble"
	"layeh.com/gumble/gumbleutil"
	_ "layeh.com/gumble/opus"
)

var (
	// Build vars
	version string
	commit  string
	date    string
)

func main() {
	log.Println("Mumble-Discord-Bridge")
	log.Println("v" + version + " " + commit + " " + date)

	godotenv.Load()

	mumbleAddr := flag.String("mumble-address", lookupEnvOrString("MUMBLE_ADDRESS", ""), "MUMBLE_ADDRESS, mumble server address, example example.com")
	mumblePort := flag.Int("mumble-port", lookupEnvOrInt("MUMBLE_PORT", 64738), "MUMBLE_PORT mumble port")
	mumbleUsername := flag.String("mumble-username", lookupEnvOrString("MUMBLE_USERNAME", "Discord"), "MUMBLE_USERNAME, mumble username")
	mumblePassword := flag.String("mumble-password", lookupEnvOrString("MUMBLE_PASSWORD", ""), "MUMBLE_PASSWORD, mumble password, optional")
	mumbleInsecure := flag.Bool("mumble-insecure", lookupEnvOrBool("MUMBLE_INSECURE", false), "mumble insecure,  env alt MUMBLE_INSECURE")
	mumbleChannel := flag.String("mumble-channel", lookupEnvOrString("MUMBLE_CHANNEL", ""), "mumble channel to start in")
	mumbleAnnounce := flag.Bool("mumble-announce", lookupEnvOrBool("MUMBLE_ANNOUNCE", true), "MUMBLE_ANNOUNCE, whether the bridge should message the Mumble text chat when a new user joins in Discord")
	discordToken := flag.String("discord-token", lookupEnvOrString("DISCORD_TOKEN", ""), "DISCORD_TOKEN, discord bot token")
	discordGID := flag.String("discord-gid", lookupEnvOrString("DISCORD_GID", ""), "DISCORD_GID, discord gid")
	discordCID := flag.String("discord-cid", lookupEnvOrString("DISCORD_CID", ""), "DISCORD_CID, discord cid")
	discordCommand := flag.String("discord-command", lookupEnvOrString("DISCORD_COMMAND", "mumble-discord"), "DISCORD_COMMAND,Discord command string, env alt DISCORD_COMMAND, optional, defaults to mumble-discord")
	mode := flag.String("mode", lookupEnvOrString("MODE", "constant"), "MODE,determine which mode the bridge starts in")
	nice := flag.Bool("nice", lookupEnvOrBool("NICE", false), "NICE,whether the bridge should automatically try to 'nice' itself")

	cpuprofile := flag.String("cpuprofile", "", "write cpu profile to `file`")

	flag.Parse()
	log.Printf("app.config %v\n", getConfig(flag.CommandLine))

	if *mumbleAddr == "" {
		log.Fatalln("missing mumble address")
	}
	if *mumbleUsername == "" {
		log.Fatalln("missing mumble username")
	}

	if *discordToken == "" {
		log.Fatalln("missing discord bot token")
	}
	if *discordGID == "" {
		log.Fatalln("missing discord gid")
	}
	if *discordCID == "" {
		log.Fatalln("missing discord cid")
	}
	if *mode == "" {
		log.Fatalln("missing mode set")
	}
	if *nice {
		err := syscall.Setpriority(syscall.PRIO_PROCESS, os.Getpid(), -5)
		if err != nil {
			log.Println("Unable to set priority. ", err)
		}
	}

	if *cpuprofile != "" {
		f, err := os.Create(*cpuprofile)
		if err != nil {
			log.Fatal("could not create CPU profile: ", err)
		}
		defer f.Close() // error handling omitted for example
		if err := pprof.StartCPUProfile(f); err != nil {
			log.Fatal("could not start CPU profile: ", err)
		}
		defer pprof.StopCPUProfile()
	}

	//Connect to discord
	discord, err := discordgo.New("Bot " + *discordToken)
	if err != nil {
		log.Println(err)
		return
	}

	// Mumble setup
	config := gumble.NewConfig()
	config.Username = *mumbleUsername
	config.Password = *mumblePassword
	config.AudioInterval = time.Millisecond * 10

	// Bridge setup
	BridgeConf := &BridgeConfig{
		Config:         config,
		MumbleAddr:     *mumbleAddr + ":" + strconv.Itoa(*mumblePort),
		MumbleInsecure: *mumbleInsecure,
		MumbleChannel:  *mumbleChannel,
		MumbleAnnounce: *mumbleAnnounce,
		Command:        *discordCommand,
		GID:            *discordGID,
		CID:            *discordCID,
	}
	Bridge := &BridgeState{
		ActiveConn:         make(chan bool),
		Connected:          false,
		DiscordUsers:       make(map[string]bool),
		MumbleChannelUsers: make(map[string]bool),
	}
	ul := &sync.Mutex{}
	cl := &sync.Mutex{}
	l := &Listener{BridgeConf, Bridge, ul, cl}

	// Discord setup
	// Open Websocket
	discord.LogLevel = 1
	discord.StateEnabled = true
	discord.Identify.Intents = discordgo.MakeIntent(discordgo.IntentsAllWithoutPrivileged)
	discord.ShouldReconnectOnError = true
	// register handlers
	discord.AddHandler(l.guildCreate)
	discord.AddHandler(l.messageCreate)
	discord.AddHandler(l.voiceUpdate)
	l.BridgeConf.Config.Attach(gumbleutil.Listener{
		Connect:    l.mumbleConnect,
		UserChange: l.mumbleUserChange,
	})

	err = discord.Open()
	if err != nil {
		log.Println(err)
		return
	}
	defer discord.Close()

	log.Println("Discord Bot Connected")
	log.Printf("Discord bot looking for command !%v", *discordCommand)

	switch *mode {
	case "auto":
		log.Println("bridge starting in automatic mode")
		Bridge.AutoChan = make(chan bool)
		Bridge.Mode = bridgeModeAuto
		go AutoBridge(discord, l)
	case "manual":
		log.Println("bridge starting in manual mode")
		Bridge.Mode = bridgeModeManual
	case "constant":
		log.Println("bridge starting in constant mode")
		Bridge.Mode = bridgeModeConstant
		go startBridge(discord, *discordGID, *discordCID, l, make(chan bool))
	default:
		discord.Close()
		log.Fatalln("invalid bridge mode set")
	}

	go discordStatusUpdate(discord, *mumbleAddr, strconv.Itoa(*mumblePort), l)
	sc := make(chan os.Signal, 1)
	signal.Notify(sc, syscall.SIGINT, syscall.SIGTERM, os.Interrupt, os.Kill)
	<-sc
	log.Println("Bot shutting down")
}
