package com.thefeed.android

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.Service
import android.content.Intent
import android.os.Build
import android.os.IBinder
import androidx.core.app.NotificationCompat
import java.io.File
import java.io.FileOutputStream

class ThefeedService : Service() {
    private var process: Process? = null

    override fun onCreate() {
        super.onCreate()
        createNotificationChannel()
        startForeground(NOTIFICATION_ID, buildNotification("Starting local service..."))
        startClientProcess()
    }

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        return START_STICKY
    }

    override fun onDestroy() {
        process?.destroy()
        process = null
        super.onDestroy()
    }

    override fun onBind(intent: Intent?): IBinder? = null

    private fun startClientProcess() {
        try {
            val bin = ensureBinary()
            val dataDir = File(filesDir, "thefeeddata")
            if (!dataDir.exists()) {
                dataDir.mkdirs()
            }

            val pb = ProcessBuilder(
                bin.absolutePath,
                "--data-dir", dataDir.absolutePath,
                "--port", "8080"
            )
            pb.redirectErrorStream(true)
            process = pb.start()

            val outputReader = process?.inputStream?.bufferedReader()
            Thread {
                try {
                    while (true) {
                        val line = outputReader?.readLine() ?: break
                        updateForegroundNotification(line)
                    }
                } catch (_: Exception) {
                }
            }.start()

            updateForegroundNotification("Running on http://127.0.0.1:8080")
        } catch (e: Exception) {
            updateForegroundNotification("Failed: ${e.message}")
            stopSelf()
        }
    }

    private fun ensureBinary(): File {
        val target = File(filesDir, "thefeed-client")
        if (target.exists() && target.length() > 0L && target.canExecute()) {
            return target
        }

        assets.open("thefeed-client").use { input ->
            FileOutputStream(target).use { out ->
                input.copyTo(out)
            }
        }

        if (!target.setExecutable(true, true)) {
            throw IllegalStateException("Could not set executable bit on bundled binary")
        }

        return target
    }

    private fun createNotificationChannel() {
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
            val channel = NotificationChannel(
                CHANNEL_ID,
                "thefeed background service",
                NotificationManager.IMPORTANCE_LOW
            )
            val manager = getSystemService(NotificationManager::class.java)
            manager.createNotificationChannel(channel)
        }
    }

    private fun buildNotification(message: String): Notification {
        return NotificationCompat.Builder(this, CHANNEL_ID)
            .setContentTitle("thefeed")
            .setContentText(message)
            .setSmallIcon(android.R.drawable.stat_notify_sync)
            .setOngoing(true)
            .build()
    }

    private fun updateForegroundNotification(message: String) {
        val manager = getSystemService(NotificationManager::class.java)
        manager.notify(NOTIFICATION_ID, buildNotification(message))
    }

    companion object {
        private const val CHANNEL_ID = "thefeed_service"
        private const val NOTIFICATION_ID = 1201
    }
}
