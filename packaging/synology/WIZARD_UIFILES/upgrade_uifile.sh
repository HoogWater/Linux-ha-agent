#!/bin/sh
# Prints the settings wizard of the package as JSON. DSM runs it for the
# upgrade wizard (pre-filled with the saved settings) and writes nothing when
# SYNOPKG_TEMP_LOGFILE is unset; build-spk.sh runs it without saved settings
# to make the install wizard.
SETTINGS=${SETTINGS:-/var/packages/LinuxHAAgent/var/settings}

setting() { sed -n "s/^$1=//p" "$SETTINGS" 2>/dev/null | head -n 1; }
json() { printf '"%s"' "$(printf '%s' "$1" | sed 's/\\/\\\\/g; s/"/\\"/g')"; }
bool() {
	case $(setting "$1") in
	true) echo true ;;
	false) echo false ;;
	*) echo "$2" ;;
	esac
}

url=$(setting mqtt_url)
[ -n "$url" ] || url=tcp://homeassistant.local:1883
if [ -s "$SETTINGS" ]; then
	password_desc="Leave empty to keep the current password."
else
	password_desc="Password of the MQTT user. Stored readable only by the package."
fi
# shellcheck disable=SC2016 # shown literally
task='T=$(readlink -f /var/packages/LinuxHAAgent/target) && chown -R root:root "$T" && /bin/sh "$T/bin/root-task.sh"'
task=$(printf '%s' "$task" | sed 's/&/\&amp;/g; s/</\&lt;/g; s/>/\&gt;/g')
root_desc="DSM runs third-party packages without root rights, but the agent needs them for Docker and Package Center. A Task Scheduler task runs it as root instead; the package's Run and Stop still control it.<br><br>If you have not done so before: in <b>Control Panel &gt; Task Scheduler</b>, choose <b>Create &gt; Triggered Task &gt; User-defined script</b>, user <b>root</b>, event <b>Boot-up</b>, and enter this command under Task Settings:<br><br><code>$task</code><br><br>Then select the task and click <b>Run</b> once, so it also runs before the next boot."

cat >"${SYNOPKG_TEMP_LOGFILE:-/dev/stdout}" <<EOF
[
  {
    "step_title": "Connect to Home Assistant",
    "items": [
      {
        "type": "textfield",
        "desc": "The agent reports this NAS to Home Assistant through its MQTT broker, for example the Mosquitto add-on. Create an MQTT user for this NAS there first.",
        "subitems": [
          {
            "key": "wizard_mqtt_url",
            "desc": "Broker URL",
            "defaultValue": $(json "$url"),
            "validator": {
              "allowBlank": false,
              "regex": {
                "expr": "/^(tcp|mqtt|ssl|tls|mqtts|ws|wss):\\\\/\\\\/[^\\\\s\\\\/]+/",
                "errorText": "Use tcp://host:1883, or ssl://host:8883 for TLS."
              }
            }
          },
          {
            "key": "wizard_mqtt_username",
            "desc": "Username",
            "defaultValue": $(json "$(setting mqtt_username)")
          }
        ]
      },
      {
        "type": "password",
        "desc": $(json "$password_desc"),
        "subitems": [
          {
            "key": "wizard_mqtt_password",
            "desc": "Password"
          }
        ]
      },
      {
        "type": "textfield",
        "desc": "Name of this NAS in Home Assistant. Leave empty to use the host name.",
        "subitems": [
          {
            "key": "wizard_host_name",
            "desc": "Device name",
            "defaultValue": $(json "$(setting host_name)")
          }
        ]
      }
    ]
  },
  {
    "step_title": "Permissions",
    "items": [
      {
        "type": "multiselect",
        "desc": "What Home Assistant may do on this NAS. DSM updates are only reported: install them in DSM.",
        "subitems": [
          {
            "key": "wizard_perm_upgrade",
            "desc": "Upgrade packages and containers",
            "defaultValue": $(bool perm_upgrade true)
          },
          {
            "key": "wizard_perm_restart_containers",
            "desc": "Restart single containers",
            "defaultValue": $(bool perm_restart_containers false)
          },
          {
            "key": "wizard_container_entities",
            "desc": "Entities for every container (running, image update, restart)",
            "defaultValue": $(bool container_entities true)
          }
        ]
      }
    ]
  },
  {
    "step_title": "Run the agent as root",
    "items": [
      {
        "type": "multiselect",
        "desc": $(json "$root_desc"),
        "subitems": [
          {
            "key": "wizard_root_task_ack",
            "desc": "I have created this task (or did so before)",
            "defaultValue": false
          }
        ]
      }
    ]
  }
]
EOF
