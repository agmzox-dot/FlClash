package com.follow.clash.common

import org.junit.Assert.assertEquals
import org.junit.Test

class ChannelNamesTest {
    @Test
    fun channelsFollowTheInstalledApplicationId() {
        assertEquals("com.follow.clash/app", channelName("com.follow.clash", "app"))
        assertEquals(
            "com.follow.clash.adaptive.candidate/service",
            channelName("com.follow.clash.adaptive.candidate", "service"),
        )
        assertEquals(
            "com.follow.clash.adaptive.candidate/tile",
            channelName("com.follow.clash.adaptive.candidate", "tile"),
        )
    }
}
